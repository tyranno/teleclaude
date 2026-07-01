//go:build windows

package main

import (
	"fmt"
	"strings"
)

// Direct !screen command handling — bypasses LLM routing/worker for fast,
// deterministic screen control from Telegram. Reuses the in-process Win32
// helpers (enumWindows / captureScreen / captureWindow / cursorPos / mouseClick)
// and the shared PresetStore. This is the Windows implementation; a stub in
// screencmd_stub.go covers other platforms.
//
// Subcommands:
//   list                    → visible top-level windows
//   shot [window]           → PNG of a window (cropped) or the full screen
//   preset save <name>      → save the current cursor position as a preset
//   click <preset>          → left-click a saved preset (no LLM)
//
// Returns (text, pngImage, error): when pngImage is non-nil the caller sends it
// as a photo with text as the caption; otherwise text is sent as a message.
func screenCommand(sub string, args []string, presetsPath string) (string, []byte, error) {
	switch strings.ToLower(strings.TrimSpace(sub)) {
	case "list":
		wins := enumWindows()
		if len(wins) == 0 {
			return "(보이는 창 없음)", nil, nil
		}
		var b strings.Builder
		for _, w := range wins {
			fmt.Fprintf(&b, "%s | hwnd=0x%x\n", w.Title, w.HWND)
		}
		return strings.TrimRight(b.String(), "\n"), nil, nil

	case "shot":
		name := strings.TrimSpace(strings.Join(args, " "))
		if name != "" {
			png, left, top, w, h, err := captureWindow(name)
			if err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("🖼 %q — %dx%d (origin %d,%d)", name, w, h, left, top), png, nil
		}
		png, err := captureScreen()
		if err != nil {
			return "", nil, err
		}
		return "🖼 전체 화면", png, nil

	case "preset":
		if len(args) < 2 || strings.ToLower(args[0]) != "save" {
			return "", nil, fmt.Errorf("사용법: !screen preset save <이름>")
		}
		name := strings.TrimSpace(strings.Join(args[1:], " "))
		if name == "" {
			return "", nil, fmt.Errorf("프리셋 이름이 필요합니다")
		}
		x, y := cursorPos()
		ps := NewPresetStore(presetsPath)
		if err := ps.Load(); err != nil {
			return "", nil, err
		}
		if err := ps.Set(name, x, y); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("💾 프리셋 %q 저장 — 현재 커서 (%d,%d)", name, x, y), nil, nil

	case "click":
		name := strings.TrimSpace(strings.Join(args, " "))
		if name == "" {
			return "", nil, fmt.Errorf("사용법: !screen click <프리셋이름>")
		}
		ps := NewPresetStore(presetsPath)
		if err := ps.Load(); err != nil {
			return "", nil, err
		}
		p, ok := ps.Get(name)
		if !ok {
			return "", nil, fmt.Errorf("프리셋 %q 없음 (먼저 !screen preset save %s)", name, name)
		}
		if err := mouseClick(p.X, p.Y, "left"); err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("🖱 프리셋 %q 클릭 — (%d,%d)", name, p.X, p.Y), nil, nil

	default:
		return "", nil, fmt.Errorf("알 수 없는 !screen 서브명령 %q (list | shot | preset | click)", sub)
	}
}
