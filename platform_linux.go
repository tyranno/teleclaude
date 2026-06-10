//go:build linux || darwin

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// applyCmdLine is a no-op on Linux/macOS — .cmd/.bat wrappers don't exist here.
func applyCmdLine(_ *exec.Cmd, _ string) {}

const exeSuffix = ""

func killTree(pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return syscall.Kill(pid, syscall.SIGKILL)
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// killByImageName sends SIGKILL to all processes matching the given name.
func killByImageName(name string) {
	exec.Command("pkill", "-f", name).Run()
}

// killPreviousInstance sends SIGTERM to the previous instance via PID file.
func killPreviousInstance() {
	myPID := os.Getpid()

	if b, err := os.ReadFile(pidFilePath()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && pid != myPID {
			if syscall.Kill(pid, syscall.SIGTERM) == nil {
				log.Printf("[main] sent SIGTERM to previous instance (PID %d)", pid)
				time.Sleep(3 * time.Second)
			}
		}
	}
}

// waitForProcessExit polls until the given PID is gone, then force-kills on timeout.
// Uses signal 0 to check process existence without killing it.
func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != nil {
			log.Printf("[main] old process (PID %d) has exited", pid)
			return
		}
	}
	syscall.Kill(pid, syscall.SIGKILL)
	log.Printf("[main] force-killed old process (PID %d) after timeout", pid)
}

// findClaudeOS returns Linux/macOS-specific candidate paths for the claude CLI.
func findClaudeOS(home string) []string {
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		"/usr/local/bin/claude",
		filepath.Join(home, ".npm-global", "bin", "claude"),
		"/usr/bin/claude",
	}
	// NVM paths: enumerate installed node versions
	nvmBase := filepath.Join(home, ".nvm", "versions", "node")
	if entries, err := os.ReadDir(nvmBase); err == nil {
		for _, e := range entries {
			candidates = append(candidates, filepath.Join(nvmBase, e.Name(), "bin", "claude"))
		}
	}
	return candidates
}
