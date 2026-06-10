//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const exeSuffix = ".exe"

func killTree(pid int) error {
	return exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// killByImageName force-kills all processes matching the given image name.
func killByImageName(name string) {
	exec.Command("taskkill", "/F", "/IM", name).Run()
}

// killPreviousInstance terminates any running teleclaude processes (except self).
func killPreviousInstance() {
	myPID := os.Getpid()
	killed := false

	if b, err := os.ReadFile(pidFilePath()); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 && pid != myPID {
			if exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run() == nil {
				log.Printf("[main] killed previous instance via PID file (PID %d)", pid)
				killed = true
			}
		}
	}

	for _, name := range []string{"teleclaude" + exeSuffix, "teleclaude_new" + exeSuffix} {
		out, _ := exec.Command("tasklist", "/FI", "IMAGENAME eq "+name, "/FO", "CSV", "/NH").CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(strings.ToLower(line), "info:") {
				continue
			}
			parts := strings.Split(line, ",")
			if len(parts) < 2 {
				continue
			}
			pid, err := strconv.Atoi(strings.Trim(parts[1], `"`))
			if err != nil || pid <= 0 || pid == myPID {
				continue
			}
			if exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run() == nil {
				log.Printf("[main] killed competing %s (PID %d)", name, pid)
				killed = true
			}
		}
	}

	if killed {
		time.Sleep(3 * time.Second)
	}
}

// waitForProcessExit polls until the given PID is gone, then force-kills on timeout.
func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		out, _ := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH").CombinedOutput()
		alive := false
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(strings.ToLower(line), "info:") {
				alive = true
				break
			}
		}
		if !alive {
			log.Printf("[main] old process (PID %d) has exited", pid)
			return
		}
	}
	exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid)).Run()
	log.Printf("[main] force-killed old process (PID %d) after timeout", pid)
}

// applyCmdLine sets cmd.SysProcAttr.CmdLine for cmd.exe /C invocation.
// This field exists only on Windows; callers must use this helper for cross-platform builds.
func applyCmdLine(cmd *exec.Cmd, cmdLine string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: cmdLine}
}

// findClaudeOS returns Windows-specific candidate paths for the claude CLI.
func findClaudeOS(home string) []string {
	return []string{
		filepath.Join(home, "AppData", "Roaming", "npm", "claude.cmd"),
		filepath.Join(home, "AppData", "Roaming", "npm", "claude.exe"),
		filepath.Join(home, ".local", "bin", "claude.exe"),
		`C:\Program Files\nodejs\claude.cmd`,
	}
}
