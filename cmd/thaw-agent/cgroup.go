//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sirupsen/logrus"
)

// cgroupManager manages cgroup v2 subtrees for isolating user processes from
// the thaw-agent. It creates two child cgroups under the thaw-agent's own
// cgroup:
//
//   - agent/  — the thaw-agent process itself (high CPU priority)
//   - user/   — all user-spawned processes via /exec and /pty (limited memory,
//     lower CPU priority)
//
// This prevents runaway user code (fork bombs, OOM, CPU spin) from starving
// the thaw-agent and making the sandbox uncontrollable.
type cgroupManager struct {
	agentDir string // e.g. /sys/fs/cgroup/.../agent
	userDir  string // e.g. /sys/fs/cgroup/.../user
	userFD   int    // open fd on userDir for UseCgroupFD
}

const (
	cgroupReserveFraction = 0.125             // 12.5% of total memory reserved for agent
	cgroupMinReserveBytes = 128 * 1024 * 1024 // 128 MiB minimum reserve
	cgroupAgentCPUWeight  = 200
	cgroupUserCPUWeight   = 50
)

// initCgroup creates the agent/ and user/ cgroup v2 subtrees and moves the
// thaw-agent into agent/. Returns nil if cgroup v2 is not available (e.g.
// cgroup v1 or not mounted), allowing graceful degradation.
func initCgroup() *cgroupManager {
	log := logrus.WithField("component", "cgroup")

	ownCgroup, err := getOwnCgroupPath()
	if err != nil {
		log.WithError(err).Info("Cannot determine own cgroup, skipping cgroup isolation")
		return nil
	}

	base := filepath.Join("/sys/fs/cgroup", ownCgroup)

	// Verify cgroup v2 is available by checking for cgroup.controllers.
	if _, err := os.Stat(filepath.Join(base, "cgroup.controllers")); err != nil {
		log.Info("cgroup v2 not available at " + base + ", skipping cgroup isolation")
		return nil
	}

	// Enable controllers in the subtree. We need cpu and memory delegated.
	if err := enableControllers(base, []string{"cpu", "memory"}); err != nil {
		log.WithError(err).Warn("Failed to enable cgroup controllers, skipping cgroup isolation")
		return nil
	}

	agentDir := filepath.Join(base, "agent")
	userDir := filepath.Join(base, "user")

	for _, dir := range []string{agentDir, userDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.WithError(err).Warn("Failed to create cgroup dir " + dir)
			return nil
		}
	}

	// Set CPU weights.
	writeCgroupFile(agentDir, "cpu.weight", strconv.Itoa(cgroupAgentCPUWeight))
	writeCgroupFile(userDir, "cpu.weight", strconv.Itoa(cgroupUserCPUWeight))

	// Set memory.high on user cgroup based on total system memory.
	if memHigh := computeUserMemoryHigh(); memHigh > 0 {
		writeCgroupFile(userDir, "memory.high", strconv.FormatInt(memHigh, 10))
		log.WithField("memory_high_bytes", memHigh).Info("Set user cgroup memory.high")
	}

	// Move the thaw-agent into the agent cgroup.
	if err := writeCgroupFile(agentDir, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
		log.WithError(err).Warn("Failed to move thaw-agent into agent cgroup")
		return nil
	}

	// Open a file descriptor on the user cgroup directory for UseCgroupFD.
	fd, err := syscall.Open(userDir, syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		log.WithError(err).Warn("Failed to open user cgroup dir for UseCgroupFD")
		return nil
	}

	log.WithFields(logrus.Fields{
		"agent_cgroup": agentDir,
		"user_cgroup":  userDir,
	}).Info("cgroup v2 isolation initialized")

	return &cgroupManager{
		agentDir: agentDir,
		userDir:  userDir,
		userFD:   fd,
	}
}

// applyCgroup configures SysProcAttr to place the child process into the user
// cgroup atomically at fork time using CLONE_INTO_CGROUP (Go 1.20+).
func (cm *cgroupManager) applyCgroup(attr *syscall.SysProcAttr) {
	if cm == nil {
		return
	}
	attr.UseCgroupFD = true
	attr.CgroupFD = cm.userFD
}

// getOwnCgroupPath reads /proc/self/cgroup to find the cgroup v2 path.
// For cgroup v2, the file contains a single line like "0::/path".
func getOwnCgroupPath() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "0::") {
			return strings.TrimPrefix(line, "0::"), nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry in /proc/self/cgroup")
}

// enableControllers writes to cgroup.subtree_control to delegate controllers
// to child cgroups.
func enableControllers(cgroupDir string, controllers []string) error {
	for _, c := range controllers {
		if err := writeCgroupFile(cgroupDir, "cgroup.subtree_control", "+"+c); err != nil {
			return fmt.Errorf("enable controller %s: %w", c, err)
		}
	}
	return nil
}

// computeUserMemoryHigh reads total system memory from /proc/meminfo and
// returns the memory.high value for the user cgroup.
func computeUserMemoryHigh() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0
			}
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			totalBytes := kb * 1024
			reserve := int64(float64(totalBytes) * cgroupReserveFraction)
			if reserve < cgroupMinReserveBytes {
				reserve = cgroupMinReserveBytes
			}
			memHigh := totalBytes - reserve
			if memHigh < 0 {
				return 0
			}
			return memHigh
		}
	}
	return 0
}

// writeCgroupFile writes a value to a cgroup control file.
func writeCgroupFile(dir, file, value string) error {
	return os.WriteFile(filepath.Join(dir, file), []byte(value), 0644)
}
