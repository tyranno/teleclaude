//go:build windows

package main

import (
	"fmt"
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
	modUser32 = windows.NewLazySystemDLL("user32.dll")

	procEnumWindows         = modUser32.NewProc("EnumWindows")
	procGetWindowTextW      = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLength = modUser32.NewProc("GetWindowTextLengthW")
	procIsWindowVisible     = modUser32.NewProc("IsWindowVisible")
	procSetForegroundWindow = modUser32.NewProc("SetForegroundWindow")
	procShowWindow          = modUser32.NewProc("ShowWindow")
	procIsIconic            = modUser32.NewProc("IsIconic")
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
	if ok, _, _ := procSetForegroundWindow.Call(target); ok == 0 {
		return fmt.Errorf("focus_window: SetForegroundWindow failed for %q", s)
	}
	return nil
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
