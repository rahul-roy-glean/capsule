package main

import (
	"encoding/binary"
	"io"
	"net/http"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

// Binary frame protocol:
// Byte 0 = type tag
// 0x00: client->server stdin data (remaining bytes)
// 0x01: server->client stdout data (remaining bytes)
// 0x02: client->server resize (uint16 cols + uint16 rows, big-endian)
// 0x03: server->client exit (int32 code, big-endian)
// 0x04: client->server signal (1 byte signal number)

const (
	msgTypeStdin  = 0x00
	msgTypeStdout = 0x01
	msgTypeResize = 0x02
	msgTypeExit   = 0x03
	msgTypeSignal = 0x04
)

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func ptyHandler(w http.ResponseWriter, r *http.Request) {
	log := logrus.WithField("handler", "pty")

	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query params
	query := r.URL.Query()
	command := query.Get("command")
	if command == "" {
		command = "/bin/bash"
	}

	cols := uint16(80)
	rows := uint16(24)
	if c := query.Get("cols"); c != "" {
		if v, err := strconv.ParseUint(c, 10, 16); err == nil && v > 0 {
			cols = uint16(v)
		}
	}
	if rr := query.Get("rows"); rr != "" {
		if v, err := strconv.ParseUint(rr, 10, 16); err == nil && v > 0 {
			rows = uint16(v)
		}
	}

	// Upgrade to WebSocket
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.WithError(err).Warn("WebSocket upgrade failed")
		return
	}
	defer conn.Close()

	// Build command
	cmd := exec.Command(command)
	cmd.Dir = *workspaceDir

	// Run as runner user (same pattern as execHandler)
	runnerUser, err := user.Lookup(*runnerUsername)
	if err != nil {
		log.WithError(err).Error("Runner user not found")
		writeWSError(conn, "runner user not found: "+err.Error())
		return
	}
	rUID, _ := strconv.ParseUint(runnerUser.Uid, 10, 32)
	rGID, _ := strconv.ParseUint(runnerUser.Gid, 10, 32)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(rUID), Gid: uint32(rGID)},
		Setsid:     true,
	}
	cmd.Env = []string{
		"TERM=xterm-256color",
		"HOME=" + runnerUser.HomeDir,
		"USER=" + runnerUser.Username,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}

	// Start PTY
	winSize := &pty.Winsize{Cols: cols, Rows: rows}
	ptmx, err := pty.StartWithSize(cmd, winSize)
	if err != nil {
		log.WithError(err).Error("Failed to start PTY")
		writeWSError(conn, "pty start failed: "+err.Error())
		return
	}
	defer ptmx.Close()

	log.WithFields(logrus.Fields{
		"pid":     cmd.Process.Pid,
		"command": command,
		"cols":    cols,
		"rows":    rows,
	}).Info("PTY session started")

	var wg sync.WaitGroup
	var connMu sync.Mutex // protects WebSocket writes

	// Read goroutine: PTY master -> 0x01 frames to client
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				frame := make([]byte, 1+n)
				frame[0] = msgTypeStdout
				copy(frame[1:], buf[:n])
				connMu.Lock()
				writeErr := conn.WriteMessage(websocket.BinaryMessage, frame)
				connMu.Unlock()
				if writeErr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Write goroutine: WebSocket frames -> dispatch on type byte
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				// Client disconnected; kill process
				if cmd.Process != nil {
					cmd.Process.Signal(syscall.SIGHUP)
				}
				return
			}
			if len(msg) == 0 {
				continue
			}
			switch msg[0] {
			case msgTypeStdin:
				if len(msg) > 1 {
					ptmx.Write(msg[1:])
				}
			case msgTypeResize:
				if len(msg) >= 5 {
					newCols := binary.BigEndian.Uint16(msg[1:3])
					newRows := binary.BigEndian.Uint16(msg[3:5])
					pty.Setsize(ptmx, &pty.Winsize{Cols: newCols, Rows: newRows})
				}
			case msgTypeSignal:
				if len(msg) >= 2 && cmd.Process != nil {
					cmd.Process.Signal(syscall.Signal(msg[1]))
				}
			}
		}
	}()

	// Wait for process to exit
	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	log.WithFields(logrus.Fields{
		"exit_code": exitCode,
		"command":   command,
	}).Info("PTY session ended")

	// Send exit frame
	exitFrame := make([]byte, 5)
	exitFrame[0] = msgTypeExit
	binary.BigEndian.PutUint32(exitFrame[1:], uint32(int32(exitCode)))
	connMu.Lock()
	conn.WriteMessage(websocket.BinaryMessage, exitFrame)
	connMu.Unlock()

	// Close WebSocket gracefully
	connMu.Lock()
	conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	connMu.Unlock()

	wg.Wait()
}

func writeWSError(conn *websocket.Conn, msg string) {
	// Send error as a stdout frame so the client can see it
	frame := make([]byte, 1+len(msg))
	frame[0] = msgTypeStdout
	copy(frame[1:], msg)
	conn.WriteMessage(websocket.BinaryMessage, frame)

	// Send exit code 1
	exitFrame := make([]byte, 5)
	exitFrame[0] = msgTypeExit
	binary.BigEndian.PutUint32(exitFrame[1:], 1)
	conn.WriteMessage(websocket.BinaryMessage, exitFrame)

	conn.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseInternalServerErr, msg))

	// Give client time to receive before close
	io.Copy(io.Discard, conn.UnderlyingConn())
}
