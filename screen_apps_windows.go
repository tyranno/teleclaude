//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Design Ref: §2 (list_windows / focus_window), §4 (screen_apps_windows.go).
//
// CGO-free Win32 window enumeration and focus via golang.org/x/sys/windows.

var (
	modUser32      = windows.NewLazySystemDLL("user32.dll")
	modKernel32App = windows.NewLazySystemDLL("kernel32.dll")

	procEnumWindows         = modUser32.NewProc("EnumWindows")
	procGetWindowTextW      = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLength = modUser32.NewProc("GetWindowTextLengthW")
	procIsWindowVisible     = modUser32.NewProc("IsWindowVisible")
	procSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")
	procShowWindow          = modUser32.NewProc("ShowWindow")
	procIsIconic            = modUser32.NewProc("IsIconic")
	procGetForegroundWindow = modUser32.NewProc("GetForegroundWindow")
	procGetWindowThreadPID  = modUser32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput   = modUser32.NewProc("AttachThreadInput")
	procBringWindowToTop    = modUser32.NewProc("BringWindowToTop")
	procGetCurrentThreadID  = modKernel32App.NewProc("GetCurrentThreadId")
)

const swRestore = 9

// win is one visible top-level window.
type win struct {
	Title string
	HWND  uintptr
}

// enumWindows returns all visible top-level windows that have a non-empty title.
func enumWindows() []win {
	var out []win

	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		// Skip invisible windows.
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1 // continue enumeration
		}

		title := getWindowText(hwnd)
		if strings.TrimSpace(title) == "" {
			return 1
		}

		out = append(out, win{Title: title, HWND: hwnd})
		return 1
	})

	procEnumWindows.Call(cb, 0)
	return out
}

// getWindowText reads a window's title via GetWindowTextW.
func getWindowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLength.Call(hwnd)
	length := int(n)
	if length <= 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	r, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:r])
}

// focusWindow brings a window to the foreground. titleOrHwnd may be either a
// hex/decimal HWND (e.g. "0x1234" or "4660") or a window-title substring
// (case-insensitive). Returns an error if no matching window is found.
func focusWindow(titleOrHwnd string) error {
	s := strings.TrimSpace(titleOrHwnd)
	if s == "" {
		return fmt.Errorf("focus_window: empty title/hwnd")
	}

	var target uintptr

	// Try to interpret the input as an HWND first.
	if h, ok := parseHWND(s); ok {
		target = h
	} else {
		// Match by title substring (case-insensitive). Prefer an exact match.
		wins := enumWindows()
		needle := strings.ToLower(s)
		var exact, partial uintptr
		for _, w := range wins {
			lt := strings.ToLower(w.Title)
			if lt == needle && exact == 0 {
				exact = w.HWND
			}
			if strings.Contains(lt, needle) && partial == 0 {
				partial = w.HWND
			}
		}
		if exact != 0 {
			target = exact
		} else {
			target = partial
		}
	}

	if target == 0 {
		return fmt.Errorf("focus_window: no window matching %q", s)
	}

	// Restore if minimized, then bring to foreground.
	if iconic, _, _ := procIsIconic.Call(target); iconic != 0 {
		procShowWindow.Call(target, swRestore)
	}
	if err := forceForeground(target); err != nil {
		return fmt.Errorf("focus_window: %w (window %q)", err, s)
	}
	return nil
}

// forceForeground brings target to the foreground, working around the Windows
// foreground-lock by attaching our input thread to the current foreground
// window's thread for the duration of the SetForegroundWindow call.
func forceForeground(target uintptr) error {
	// Fast path.
	if ok, _, _ := procSetForegroundWindow.Call(target); ok != 0 {
		return nil
	}

	fg, _, _ := procGetForegroundWindow.Call()
	curTID, _, _ := procGetCurrentThreadID.Call()
	fgTID, _, _ := procGetWindowThreadPID.Call(fg, 0)
	tgtTID, _, _ := procGetWindowThreadPID.Call(target, 0)

	// Attach our thread (and the target's) to the foreground thread so the OS
	// permits the foreground change.
	if fgTID != 0 && fgTID != curTID {
		procAttachThreadInput.Call(curTID, fgTID, 1)
		defer procAttachThreadInput.Call(curTID, fgTID, 0)
	}
	if tgtTID != 0 && tgtTID != curTID && tgtTID != fgTID {
		procAttachThreadInput.Call(curTID, tgtTID, 1)
		defer procAttachThreadInput.Call(curTID, tgtTID, 0)
	}

	procBringWindowToTop.Call(target)
	procShowWindow.Call(target, swRestore)
	if ok, _, _ := procSetForegroundWindow.Call(target); ok != 0 {
		return nil
	}
	return fmt.Errorf("SetForegroundWindow failed")
}

// appAliases maps common app names to a concrete executable that works
// regardless of the Windows UI language (e.g. on Korean Windows the Calculator
// Start Menu shortcut is "계산기", not "Calculator", so a name search misses it).
// Keys are lower-cased.
var appAliases = map[string]string{
	"calculator":     "calc.exe",
	"calc":           "calc.exe",
	"notepad":        "notepad.exe",
	"메모장":            "notepad.exe",
	"explorer":       "explorer.exe",
	"file explorer":  "explorer.exe",
	"파일 탐색기":         "explorer.exe",
	"cmd":            "cmd.exe",
	"command prompt": "cmd.exe",
	"paint":          "mspaint.exe",
	"task manager":   "taskmgr.exe",
}

// launchApp finds and launches a Windows application BY NAME and returns a
// human-readable description of which fallback path succeeded.
//
// Fallback chain (first that launches wins):
//  1. Built-in alias map for common Windows apps (language-independent).
//  2. Start Menu *.lnk search (per-user + machine-wide, recursive) whose base
//     name contains the given name case-insensitively.
//  3. PATH lookup of <name> / <name>.exe, launched via the shell.
//  4. Last resort: `start "" "<name>"` to let the shell resolve it.
//
// On total failure it returns an error listing what was tried — it never relies
// on a path that pops a GUI "not found" dialog without also reporting failure.
//
// CGO-free; uses os/exec.
func launchApp(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("launch_app: empty name")
	}

	var tried []string

	// 1) Alias map (language-independent executables).
	if exeName, ok := appAliases[strings.ToLower(name)]; ok {
		if err := runDetached(exeName); err == nil {
			return fmt.Sprintf("launched via alias map: %s -> %s", name, exeName), nil
		}
		tried = append(tried, "alias "+exeName)
	}

	// 2) Start Menu shortcut search.
	if lnk := findStartMenuShortcut(name); lnk != "" {
		if err := shellStart(lnk); err == nil {
			return fmt.Sprintf("launched Start Menu shortcut: %s", lnk), nil
		}
		tried = append(tried, "shortcut "+lnk)
	} else {
		tried = append(tried, "no Start Menu .lnk match")
	}

	// 3) PATH lookup of <name> and <name>.exe.
	for _, cand := range []string{name, name + ".exe"} {
		if p, err := exec.LookPath(cand); err == nil {
			if rerr := runDetached(p); rerr == nil {
				return fmt.Sprintf("launched via PATH: %s", p), nil
			}
			tried = append(tried, "PATH "+p)
		}
	}

	// 4) Last resort: shell resolve (covers UWP App Paths etc.).
	if err := shellStart(name); err == nil {
		return fmt.Sprintf("launched via shell resolve: %s", name), nil
	}
	tried = append(tried, "shell start "+name)

	return "", fmt.Errorf("launch_app: could not launch %q; tried: %s", name, strings.Join(tried, "; "))
}

// runDetached starts an executable directly (no shell), detached from this
// process, returning an error if the process could not be started.
func runDetached(exePath string) error {
	return exec.Command(exePath).Start()
}

// startMenuDirs returns the per-user and machine-wide Start Menu Programs roots.
func startMenuDirs() []string {
	var dirs []string
	if appData := os.Getenv("APPDATA"); appData != "" {
		dirs = append(dirs, filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	if programData := os.Getenv("ProgramData"); programData != "" {
		dirs = append(dirs, filepath.Join(programData, "Microsoft", "Windows", "Start Menu", "Programs"))
	}
	return dirs
}

// findStartMenuShortcut walks the Start Menu Programs folders and returns the
// path of the first *.lnk whose base name contains name (case-insensitive).
func findStartMenuShortcut(name string) string {
	needle := strings.ToLower(name)
	for _, root := range startMenuDirs() {
		var found string
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if !strings.EqualFold(filepath.Ext(path), ".lnk") {
				return nil
			}
			base := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
			if strings.Contains(base, needle) {
				found = path
				return filepath.SkipAll
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	return ""
}

// shellStart launches a target (a .lnk path or an app name) via the shell's
// `start` verb, so shortcuts and registered app names both resolve. The empty
// "" arg is the window TITLE that `start` requires before the target.
func shellStart(target string) error {
	return exec.Command("cmd", "/c", "start", "", target).Start()
}

// parseHWND parses "0x.." (hex) or a decimal string into an HWND value.
func parseHWND(s string) (uintptr, bool) {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, err := strconv.ParseUint(s[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		return uintptr(v), true
	}
	// Plain decimal — only treat as HWND if it's all digits.
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		return uintptr(v), true
	}
	return 0, false
}
