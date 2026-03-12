// pty-client connects to a runner's PTY WebSocket and provides an interactive
// terminal session. It speaks the binary frame protocol used by the capsule-thaw-agent:
//
//	0x00  client→server  stdin
//	0x01  server→client  stdout
//	0x02  client→server  resize (uint16 cols + uint16 rows, big-endian)
//	0x03  server→client  exit   (int32 code, big-endian)
//
// Usage:
//
//	pty-client -runner <id> [-host localhost:9080] [-cols 120] [-rows 40] [-command /bin/bash]
package main

import (
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

func main() {
	host := "localhost:9080"
	runnerID := ""
	cols := 0
	rows := 0
	command := "/bin/bash"

	// Minimal flag parsing to avoid pulling in flag package noise.
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-runner":
			i++
			runnerID = args[i]
		case "-host":
			i++
			host = args[i]
		case "-cols":
			i++
			cols, _ = strconv.Atoi(args[i])
		case "-rows":
			i++
			rows, _ = strconv.Atoi(args[i])
		case "-command":
			i++
			command = args[i]
		default:
			// If no flag prefix, treat as runner ID for convenience.
			if runnerID == "" {
				runnerID = args[i]
			}
		}
	}

	if runnerID == "" {
		fmt.Fprintf(os.Stderr, "Usage: pty-client -runner <id> [-host localhost:9080] [-command /bin/bash]\n")
		os.Exit(1)
	}

	// Detect terminal size if not specified.
	if cols == 0 || rows == 0 {
		w, h, err := term.GetSize(int(os.Stdin.Fd()))
		if err == nil {
			cols, rows = w, h
		} else {
			cols, rows = 120, 40
		}
	}

	// Build WebSocket URL.
	u := url.URL{
		Scheme:   "ws",
		Host:     host,
		Path:     fmt.Sprintf("/api/v1/runners/%s/pty", runnerID),
		RawQuery: fmt.Sprintf("cols=%d&rows=%d&command=%s", cols, rows, url.QueryEscape(command)),
	}

	fmt.Fprintf(os.Stderr, "Connecting to %s ...\n", u.String())

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WebSocket dial failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Put terminal in raw mode.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to set raw mode: %v\n", err)
		os.Exit(1)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	done := make(chan struct{})

	// Handle SIGWINCH for terminal resize.
	sigwinch := make(chan os.Signal, 1)
	signal.Notify(sigwinch, syscall.SIGWINCH)
	go func() {
		for range sigwinch {
			w, h, err := term.GetSize(int(os.Stdin.Fd()))
			if err != nil {
				continue
			}
			frame := make([]byte, 5)
			frame[0] = 0x02
			binary.BigEndian.PutUint16(frame[1:3], uint16(w))
			binary.BigEndian.PutUint16(frame[3:5], uint16(h))
			conn.WriteMessage(websocket.BinaryMessage, frame)
		}
	}()

	// Read from WebSocket → stdout.
	go func() {
		defer close(done)
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if len(msg) < 1 {
				continue
			}
			switch msg[0] {
			case 0x01: // stdout
				os.Stdout.Write(msg[1:])
			case 0x03: // exit
				if len(msg) >= 5 {
					code := int(binary.BigEndian.Uint32(msg[1:5]))
					fmt.Fprintf(os.Stderr, "\r\n[Process exited with code %d]\r\n", code)
				}
				return
			}
		}
	}()

	// Read from stdin → WebSocket.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil || n == 0 {
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			frame := make([]byte, 1+n)
			frame[0] = 0x00
			copy(frame[1:], buf[:n])
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		}
	}()

	<-done
}
