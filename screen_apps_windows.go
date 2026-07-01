//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Design Ref: §2 (list_windows / focus_window), §4 (screen_apps_windows.go).
//
// CGO-free Win32 window enumeration and focus via golang.org/x/sys/windows.

var (
	modUser32      = windows.NewLazySystemDLL("user32.dll")
	modKernel32App = windows.NewLazySystemDLL("kernel32.dll")

	procEnumWindows          = modUser32.NewProc("EnumWindows")
	procEnumChildWindows     = modUser32.NewProc("EnumChildWindows")
	procGetWindowTextW       = modUser32.NewProc("GetWindowTextW")
	procGetWindowTextLength  = modUser32.NewProc("GetWindowTextLengthW")
	procGetClassNameW        = modUser32.NewProc("GetClassNameW")
	procGetWindowRect        = modUser32.NewProc("GetWindowRect")
	procIsWindowVisible      = modUser32.NewProc("IsWindowVisible")
	procSetForegroundWindow  = modUser32.NewProc("SetForegroundWindow")
	procShowWindow           = modUser32.NewProc("ShowWindow")
	procIsIconic             = modUser32.NewProc("IsIconic")
	procGetForegroundWindow  = modUser32.NewProc("GetForegroundWindow")
	procGetWindowThreadPID   = modUser32.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput    = modUser32.NewProc("AttachThreadInput")
	procBringWindowToTop     = modUser32.NewProc("BringWindowToTop")
	procSystemParametersInfo = modUser32.NewProc("SystemParametersInfoW")
	procLockSetForeground    = modUser32.NewProc("LockSetForegroundWindow")
	procGetCurrentThreadID   = modKernel32App.NewProc("GetCurrentThreadId")
)

// originAnchor remembers a NON-pinned window on the desktop that was active when
// the screen MCP first switched to another virtual desktop (to operate a window
// there). return_desktop re-focuses it to switch back. Guarded by originMu.
var (
	originMu     sync.Mutex
	originAnchor uintptr
)

// clearForegroundLock disables the Windows foreground-lock (timeout→0, unlock) so
// SetForegroundWindow from this background process reliably switches focus — and,
// crucially, switches virtual desktops — even when another app is foreground.
func clearForegroundLock() {
	procSystemParametersInfo.Call(spiSetFgLock, 0, 0, spifSendChange)
	procLockSetForeground.Call(lsfwUnlock)
}

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
	target, ok := findTopWindow(s)
	if !ok {
		return fmt.Errorf("focus_window: no window matching %q", s)
	}
	if err := bringToFront(target); err != nil {
		return fmt.Errorf("focus_window: %w (window %q)", err, s)
	}
	return nil
}

// findTopWindow resolves titleOrHwnd to a top-level window handle. The input may
// be an hwnd ("0x1234"/decimal) or a title substring (case-insensitive); an
// exact title match wins over a partial one.
func findTopWindow(titleOrHwnd string) (uintptr, bool) {
	s := strings.TrimSpace(titleOrHwnd)
	if s == "" {
		return 0, false
	}
	if h, ok := parseHWND(s); ok {
		return h, true
	}
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
		return exact, true
	}
	if partial != 0 {
		return partial, true
	}
	return 0, false
}

// bringToFront restores (if minimized) and foregrounds a window handle.
// If the target is on another virtual desktop, focusing it makes Windows switch
// to that desktop — so we first remember a return anchor on the current desktop
// (once), letting return_desktop switch back afterwards.
func bringToFront(hwnd uintptr) error {
	if isWindowOnAnotherDesktop(hwnd) {
		originMu.Lock()
		if originAnchor == 0 {
			if a := pickReturnAnchor(); a != 0 && a != hwnd {
				originAnchor = a
			}
		}
		originMu.Unlock()
	}
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	return forceForeground(hwnd)
}

// returnToOriginDesktop switches back to the virtual desktop that was active
// before the screen MCP first jumped to another desktop, by re-focusing the
// remembered anchor window. No-op (with a message) if no switch happened.
func returnToOriginDesktop() (string, error) {
	originMu.Lock()
	anchor := originAnchor
	originAnchor = 0
	originMu.Unlock()

	if anchor == 0 {
		return "no saved origin desktop (no cross-desktop switch happened)", nil
	}
	if err := forceForeground(anchor); err != nil {
		return "", fmt.Errorf("return_desktop: %w", err)
	}
	return fmt.Sprintf("returned to origin desktop (re-focused %q)", getWindowText(anchor)), nil
}

// rect mirrors the Win32 RECT struct (screen coordinates).
type rect struct{ Left, Top, Right, Bottom int32 }

// control is one Win32 child-window control with its screen rectangle. For many
// native apps (incl. NetGuard Lite) the real interactive controls are standard
// child windows even when UIA exposes nothing — so EnumChildWindows yields exact
// coordinates and labels without any vision/coordinate guessing.
type control struct {
	Class                    string
	Text                     string
	Left, Top, Right, Bottom int32
	Visible                  bool
}

// CenterX/CenterY are the click target for the control.
func (c control) CenterX() int { return int((c.Left + c.Right) / 2) }
func (c control) CenterY() int { return int((c.Top + c.Bottom) / 2) }

// getClassName reads a window's class name (e.g. "Button", "SysTreeView32").
func getClassName(hwnd uintptr) string {
	buf := make([]uint16, 256)
	r, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return windows.UTF16ToString(buf[:r])
}

// windowRect returns a window's screen rectangle via GetWindowRect.
func windowRect(hwnd uintptr) rect {
	var rc rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))
	return rc
}

// enumControls returns the descendant child-window controls of a top-level
// window. When includeHidden is false only currently-visible controls (the
// active panel/tab) are returned, which keeps the list small and token-cheap.
func enumControls(top uintptr, includeHidden bool) []control {
	var out []control
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		vis := false
		if v, _, _ := procIsWindowVisible.Call(hwnd); v != 0 {
			vis = true
		}
		if !vis && !includeHidden {
			return 1 // continue
		}
		rc := windowRect(hwnd)
		out = append(out, control{
			Class:   getClassName(hwnd),
			Text:    getWindowText(hwnd),
			Left:    rc.Left,
			Top:     rc.Top,
			Right:   rc.Right,
			Bottom:  rc.Bottom,
			Visible: vis,
		})
		return 1
	})
	procEnumChildWindows.Call(top, cb, 0)
	return out
}

// listControls resolves a window then returns its child controls.
//
// A minimized window reports every child's rect as the Windows iconic
// placeholder (-32000,-32000, 0x0), which looks like valid-but-useless data
// rather than an error — so we reject it explicitly instead of returning
// coordinates that silently click nowhere.
func listControls(window string, includeHidden bool) ([]control, error) {
	top, ok := findTopWindow(window)
	if !ok {
		return nil, fmt.Errorf("no window matching %q", window)
	}
	if iconic, _, _ := procIsIconic.Call(top); iconic != 0 {
		return nil, fmt.Errorf("window %q is minimized (control rects would be bogus); focus_window it first", window)
	}
	return enumControls(top, includeHidden), nil
}

// windowPID returns the process id that owns hwnd.
func windowPID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

// defaultAffirmative are the button labels treated as "confirm/proceed" when
// auto-handling dialogs (Korean + English). Matched by EXACT normalized text (not
// substring) so a short token never matches an unintended button — e.g. "보내"
// must NOT match "내보내기"(Export), and "예" must NOT match "예약"(reserve).
var defaultAffirmative = []string{"예", "확인", "yes", "ok", "전송", "적용", "apply"}

// normalizeButtonText strips a trailing accelerator parenthetical and "&" markers
// ("예(&Y)" -> "예", "&Yes" -> "yes") and lowercases, so affirmative matching can
// be an EXACT comparison rather than a loose substring.
func normalizeButtonText(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = s[:i]
	}
	s = strings.ReplaceAll(s, "&", "")
	return strings.ToLower(strings.TrimSpace(s))
}

// findAffirmativeButton returns the first VISIBLE Button control in hwnd whose
// normalized label EXACTLY equals one of accept (accelerator/case-insensitive).
func findAffirmativeButton(hwnd uintptr, accept []string) (control, bool) {
	want := make(map[string]bool, len(accept))
	for _, a := range accept {
		if n := normalizeButtonText(a); n != "" {
			want[n] = true
		}
	}
	for _, c := range enumControls(hwnd, false) {
		if !strings.Contains(strings.ToLower(c.Class), "button") {
			continue
		}
		if want[normalizeButtonText(c.Text)] {
			return c, true
		}
	}
	return control{}, false
}

// confirmDialogs auto-clicks affirmative buttons on confirmation dialogs that
// pop up for appTitle, so an automated sweep runs continuously without human
// input (e.g. the app's "전송하시겠습니까?" prompts). It handles up to maxN
// consecutive dialogs — separate popup windows owned by the app's process, or a
// dialog rendered as visible child controls of the main window — and returns one
// line per dialog handled. accept is the affirmative-label list (defaults apply
// when empty).
func confirmDialogs(appTitle string, accept []string, maxN int) ([]string, error) {
	main, ok := findTopWindow(appTitle)
	if !ok {
		return nil, fmt.Errorf("no window matching %q", appTitle)
	}
	if len(accept) == 0 {
		accept = defaultAffirmative
	}
	if maxN <= 0 {
		maxN = 5
	}
	pid := windowPID(main)

	var handled []string
	for i := 0; i < maxN; i++ {
		clicked := false

		// 1) Separate popup window belonging to the same app process.
		for _, w := range enumWindows() {
			if w.HWND == main || windowPID(w.HWND) != pid {
				continue
			}
			if btn, found := findAffirmativeButton(w.HWND, accept); found {
				_ = bringToFront(w.HWND)
				if err := mouseClick(btn.CenterX(), btn.CenterY(), "left"); err == nil {
					handled = append(handled, fmt.Sprintf("popup %q -> clicked %q", w.Title, btn.Text))
					clicked = true
					break
				}
			}
		}

		// 2) Dialog rendered as visible child controls of the main window.
		if !clicked {
			if btn, found := findAffirmativeButton(main, accept); found {
				if err := mouseClick(btn.CenterX(), btn.CenterY(), "left"); err == nil {
					handled = append(handled, fmt.Sprintf("inline dialog -> clicked %q", btn.Text))
					clicked = true
				}
			}
		}

		if !clicked {
			break // no more dialogs
		}
		time.Sleep(400 * time.Millisecond) // let the next dialog (if any) appear
	}
	return handled, nil
}

// clickControl finds the nth (0-based) VISIBLE control in window whose label
// contains text (case-insensitive) and left-clicks its center. The window is
// brought to the foreground first so the click lands on it.
func clickControl(window, text string, nth int) (string, error) {
	top, ok := findTopWindow(window)
	if !ok {
		return "", fmt.Errorf("no window matching %q", window)
	}
	if err := bringToFront(top); err != nil {
		return "", fmt.Errorf("focus %q: %w", window, err)
	}
	needle := strings.ToLower(strings.TrimSpace(text))
	if needle == "" {
		return "", fmt.Errorf("empty control text")
	}
	var matches []control
	for _, c := range enumControls(top, false) {
		if strings.Contains(strings.ToLower(c.Text), needle) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no visible control with text containing %q in %q", text, window)
	}
	if nth < 0 || nth >= len(matches) {
		nth = 0
	}
	c := matches[nth]
	if err := mouseClick(c.CenterX(), c.CenterY(), "left"); err != nil {
		return "", err
	}
	return fmt.Sprintf("clicked %s %q at (%d,%d) [%d match(es); nth=%d]%s",
		c.Class, c.Text, c.CenterX(), c.CenterY(), len(matches), nth, uipiWarning(top)), nil
}

// forceForeground brings target to the foreground, working around the Windows
// foreground-lock by attaching our input thread to the current foreground
// window's thread for the duration of the SetForegroundWindow call.
func forceForeground(target uintptr) error {
	// Drop the foreground lock first so focus/desktop switches are not silently
	// refused when another app currently owns the foreground.
	clearForegroundLock()

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
// When elevated is true the app is launched via the "runas" verb (UAC prompt the
// user approves), so it runs with administrator rights. NOTE: to then CLICK an
// elevated app, teleclaude itself must also be elevated (Windows UIPI) — the
// cleanest setup is screen_control.elevated, after which normal launches already
// inherit elevation and this flag is unnecessary.
//
// CGO-free; uses os/exec.
func launchApp(name string, elevated bool) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("launch_app: empty name")
	}

	if elevated {
		// Resolve the best concrete target, then launch it elevated via runas.
		target := name
		if exeName, ok := appAliases[strings.ToLower(name)]; ok {
			target = exeName
		} else if lnk := findStartMenuShortcut(name); lnk != "" {
			target = lnk
		} else if p, err := exec.LookPath(name); err == nil {
			target = p
		} else if p, err := exec.LookPath(name + ".exe"); err == nil {
			target = p
		}
		if err := runAsAdmin(target, ""); err != nil {
			return "", fmt.Errorf("launch_app(elevated): runas %q failed: %w", target, err)
		}
		return fmt.Sprintf("launched as administrator (UAC) : %s", target), nil
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
