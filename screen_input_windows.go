//go:build windows

package main

import (
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Design Ref: §2 (click/move/type/key/scroll), §4 (screen_input_windows.go).
//
// CGO-free mouse/keyboard input via Win32 SendInput (golang.org/x/sys/windows).
// All coordinate-based ops normalize to the 65535 virtual-screen space and use
// MOUSEEVENTF_ABSOLUTE|MOUSEEVENTF_VIRTUALDESK so they work across multiple
// monitors with the same origin/scale as the screenshot capture.

var (
	modUser32In = windows.NewLazySystemDLL("user32.dll")

	procSendInput          = modUser32In.NewProc("SendInput")
	procGetSystemMetricsIn = modUser32In.NewProc("GetSystemMetrics")
	procVkKeyScanW         = modUser32In.NewProc("VkKeyScanW")
	procMapVirtualKeyW     = modUser32In.NewProc("MapVirtualKeyW")
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	// Mouse event flags.
	mouseeventfMove        = 0x0001
	mouseeventfLeftDown    = 0x0002
	mouseeventfLeftUp      = 0x0004
	mouseeventfRightDown   = 0x0008
	mouseeventfRightUp     = 0x0010
	mouseeventfMiddleDown  = 0x0020
	mouseeventfMiddleUp    = 0x0040
	mouseeventfWheel       = 0x0800
	mouseeventfHWheel      = 0x1000
	mouseeventfAbsolute    = 0x8000
	mouseeventfVirtualDesk = 0x4000

	// Keyboard event flags.
	keyeventfKeyUp   = 0x0002
	keyeventfUnicode = 0x0004

	wheelDelta = 120

	// System metrics for the virtual screen (same as capture).
	inSmXVirtualScreen  = 76
	inSmYVirtualScreen  = 77
	inSmCXVirtualScreen = 78
	inSmCYVirtualScreen = 79
)

// Common virtual-key codes (for keyCombo). Lowercase keys.
var vkMap = map[string]uint16{
	"enter":     0x0D,
	"return":    0x0D,
	"tab":       0x09,
	"esc":       0x1B,
	"escape":    0x1B,
	"space":     0x20,
	"backspace": 0x08,
	"bksp":      0x08,
	"delete":    0x2E,
	"del":       0x2E,
	"insert":    0x2D,
	"home":      0x24,
	"end":       0x23,
	"pageup":    0x21,
	"pgup":      0x21,
	"pagedown":  0x22,
	"pgdn":      0x22,
	"up":        0x26,
	"down":      0x28,
	"left":      0x25,
	"right":     0x27,
	"f1":        0x70,
	"f2":        0x71,
	"f3":        0x72,
	"f4":        0x73,
	"f5":        0x74,
	"f6":        0x75,
	"f7":        0x76,
	"f8":        0x77,
	"f9":        0x78,
	"f10":       0x79,
	"f11":       0x7A,
	"f12":       0x7B,
}

// Modifier virtual-key codes.
var modVK = map[string]uint16{
	"ctrl":    0x11, // VK_CONTROL
	"control": 0x11,
	"alt":     0x12, // VK_MENU
	"shift":   0x10, // VK_SHIFT
	"win":     0x5B, // VK_LWIN
	"super":   0x5B,
	"meta":    0x5B,
	"cmd":     0x5B,
}

// mouseInput mirrors the MOUSEINPUT portion of the Win32 INPUT union. The struct
// below is sized for the mouse case (the largest of the union members on amd64).
type mouseInputBlock struct {
	Type uint32
	_    uint32 // padding to 8-byte alignment on amd64
	Mi   struct {
		Dx          int32
		Dy          int32
		MouseData   uint32
		DwFlags     uint32
		Time        uint32
		DwExtraInfo uintptr
	}
}

// keybdInputBlock mirrors the KEYBDINPUT case of the Win32 INPUT union, padded to
// the same overall size as the mouse case so a homogeneous []input array works.
type keybdInputBlock struct {
	Type uint32
	_    uint32
	Ki   struct {
		WVk         uint16
		WScan       uint16
		DwFlags     uint32
		Time        uint32
		DwExtraInfo uintptr
		_           uint32 // pad to match mouseInputBlock size
		_           uint32
	}
}

// sendInputs sends a contiguous block of INPUT structures. Each entry must be
// exactly sizeof(mouseInputBlock) bytes (the union size we standardize on).
func sendInputs(raw []byte, count int) error {
	if count == 0 {
		return nil
	}
	size := unsafe.Sizeof(mouseInputBlock{})
	n, _, err := procSendInput.Call(
		uintptr(count),
		uintptr(unsafe.Pointer(&raw[0])),
		size,
	)
	if int(n) != count {
		return fmt.Errorf("SendInput sent %d of %d events: %v", int(n), count, err)
	}
	return nil
}

// mouseEvent builds a single mouse INPUT block (union-sized) as raw bytes.
func mouseEvent(dx, dy int32, mouseData uint32, flags uint32) []byte {
	var blk mouseInputBlock
	blk.Type = inputMouse
	blk.Mi.Dx = dx
	blk.Mi.Dy = dy
	blk.Mi.MouseData = mouseData
	blk.Mi.DwFlags = flags
	return structBytes(unsafe.Pointer(&blk))
}

// keyEvent builds a single keyboard INPUT block (union-sized) as raw bytes.
func keyEvent(vk, scan uint16, flags uint32) []byte {
	var blk keybdInputBlock
	blk.Type = inputKeyboard
	blk.Ki.WVk = vk
	blk.Ki.WScan = scan
	blk.Ki.DwFlags = flags
	return structBytes(unsafe.Pointer(&blk))
}

// structBytes copies a union-sized INPUT block into a fresh byte slice.
func structBytes(p unsafe.Pointer) []byte {
	size := int(unsafe.Sizeof(mouseInputBlock{}))
	out := make([]byte, size)
	copy(out, unsafe.Slice((*byte)(p), size))
	return out
}

// toAbsolute converts a physical pixel coordinate on the virtual desktop to the
// 0..65535 normalized space SendInput expects with MOUSEEVENTF_ABSOLUTE.
func toAbsolute(x, y int) (int32, int32) {
	originX, _, _ := procGetSystemMetricsIn.Call(inSmXVirtualScreen)
	originY, _, _ := procGetSystemMetricsIn.Call(inSmYVirtualScreen)
	w, _, _ := procGetSystemMetricsIn.Call(inSmCXVirtualScreen)
	h, _, _ := procGetSystemMetricsIn.Call(inSmCYVirtualScreen)

	width := int(int32(w))
	height := int(int32(h))
	ox := int(int32(originX))
	oy := int(int32(originY))
	if width <= 1 {
		width = 1
	}
	if height <= 1 {
		height = 1
	}

	// Normalize relative to the virtual-screen origin. The +1 / (w-1) form maps
	// the last pixel to 65535 to reduce off-by-one drift on the far edge.
	nx := int64(x-ox) * 65535 / int64(width-1)
	ny := int64(y-oy) * 65535 / int64(height-1)
	if nx < 0 {
		nx = 0
	}
	if nx > 65535 {
		nx = 65535
	}
	if ny < 0 {
		ny = 0
	}
	if ny > 65535 {
		ny = 65535
	}
	return int32(nx), int32(ny)
}

// mouseMove moves the cursor to absolute (x,y) on the virtual desktop.
func mouseMove(x, y int) error {
	ensureDPIAware()
	ax, ay := toAbsolute(x, y)
	flags := uint32(mouseeventfMove | mouseeventfAbsolute | mouseeventfVirtualDesk)
	raw := mouseEvent(ax, ay, 0, flags)
	return sendInputs(raw, 1)
}

// mouseClick moves to (x,y) then performs a down+up of the given button.
// button is one of "left", "right", "middle" (default "left").
func mouseClick(x, y int, button string) error {
	ensureDPIAware()

	var down, up uint32
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "", "left":
		down, up = mouseeventfLeftDown, mouseeventfLeftUp
	case "right":
		down, up = mouseeventfRightDown, mouseeventfRightUp
	case "middle":
		down, up = mouseeventfMiddleDown, mouseeventfMiddleUp
	default:
		return fmt.Errorf("unknown mouse button %q (want left/right/middle)", button)
	}

	ax, ay := toAbsolute(x, y)
	abs := uint32(mouseeventfAbsolute | mouseeventfVirtualDesk)

	var buf []byte
	buf = append(buf, mouseEvent(ax, ay, 0, mouseeventfMove|abs)...)
	buf = append(buf, mouseEvent(ax, ay, 0, down|abs)...)
	buf = append(buf, mouseEvent(ax, ay, 0, up|abs)...)
	return sendInputs(buf, 3)
}

// resolveButton maps a button name to its down/up MOUSEEVENTF flags.
func resolveButton(button string) (down, up uint32, err error) {
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "", "left":
		return mouseeventfLeftDown, mouseeventfLeftUp, nil
	case "right":
		return mouseeventfRightDown, mouseeventfRightUp, nil
	case "middle":
		return mouseeventfMiddleDown, mouseeventfMiddleUp, nil
	default:
		return 0, 0, fmt.Errorf("unknown mouse button %q (want left/right/middle)", button)
	}
}

// mouseClickMods clicks at (x,y) while holding the given modifier keys (any of
// ctrl/alt/shift/win) down — e.g. ctrl+click or shift+click for multi-select.
// Keyboard and mouse events are sent as one ordered SendInput batch.
func mouseClickMods(x, y int, button string, mods []string) error {
	ensureDPIAware()
	down, up, err := resolveButton(button)
	if err != nil {
		return err
	}
	var modVKs []uint16
	for _, m := range mods {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" {
			continue
		}
		vk, ok := modVK[m]
		if !ok {
			return fmt.Errorf("unknown modifier %q (want ctrl/alt/shift/win)", m)
		}
		modVKs = append(modVKs, vk)
	}
	if len(modVKs) == 0 {
		return mouseClick(x, y, button)
	}

	ax, ay := toAbsolute(x, y)
	abs := uint32(mouseeventfAbsolute | mouseeventfVirtualDesk)

	var buf []byte
	count := 0
	for _, vk := range modVKs {
		buf = append(buf, keyEvent(vk, 0, 0)...)
		count++
	}
	buf = append(buf, mouseEvent(ax, ay, 0, mouseeventfMove|abs)...)
	buf = append(buf, mouseEvent(ax, ay, 0, down|abs)...)
	buf = append(buf, mouseEvent(ax, ay, 0, up|abs)...)
	count += 3
	for i := len(modVKs) - 1; i >= 0; i-- {
		buf = append(buf, keyEvent(modVKs[i], 0, keyeventfKeyUp)...)
		count++
	}
	return sendInputs(buf, count)
}

// mouseDrag presses button at (x1,y1), moves through interpolated steps to
// (x2,y2), then releases — for rubber-band selection, sliders, and drag & drop.
// Small delays between phases make the gesture register reliably across apps.
func mouseDrag(x1, y1, x2, y2 int, button string) error {
	ensureDPIAware()
	down, up, err := resolveButton(button)
	if err != nil {
		return err
	}
	abs := uint32(mouseeventfAbsolute | mouseeventfVirtualDesk)

	ax1, ay1 := toAbsolute(x1, y1)
	if err := sendInputs(mouseEvent(ax1, ay1, 0, mouseeventfMove|abs), 1); err != nil {
		return err
	}
	time.Sleep(15 * time.Millisecond)
	if err := sendInputs(mouseEvent(ax1, ay1, 0, down|abs), 1); err != nil {
		return err
	}
	time.Sleep(15 * time.Millisecond)

	const steps = 12
	for i := 1; i <= steps; i++ {
		xi := x1 + (x2-x1)*i/steps
		yi := y1 + (y2-y1)*i/steps
		axi, ayi := toAbsolute(xi, yi)
		if err := sendInputs(mouseEvent(axi, ayi, 0, mouseeventfMove|abs), 1); err != nil {
			return err
		}
		time.Sleep(8 * time.Millisecond)
	}

	ax2, ay2 := toAbsolute(x2, y2)
	time.Sleep(15 * time.Millisecond)
	return sendInputs(mouseEvent(ax2, ay2, 0, up|abs), 1)
}

// mouseDouble performs a left double-click at (x,y).
func mouseDouble(x, y int) error {
	ensureDPIAware()
	ax, ay := toAbsolute(x, y)
	abs := uint32(mouseeventfAbsolute | mouseeventfVirtualDesk)

	var buf []byte
	buf = append(buf, mouseEvent(ax, ay, 0, mouseeventfMove|abs)...)
	for i := 0; i < 2; i++ {
		buf = append(buf, mouseEvent(ax, ay, 0, mouseeventfLeftDown|abs)...)
		buf = append(buf, mouseEvent(ax, ay, 0, mouseeventfLeftUp|abs)...)
	}
	return sendInputs(buf, 5)
}

// typeText types a Unicode string by emitting KEYEVENTF_UNICODE down/up pairs per
// UTF-16 code unit. This bypasses the keyboard layout, so any printable rune
// (including non-ASCII) is entered verbatim.
func typeText(s string) error {
	ensureDPIAware()
	if s == "" {
		return nil
	}
	units := windows.StringToUTF16(s)
	// Drop the trailing NUL terminator.
	if n := len(units); n > 0 && units[n-1] == 0 {
		units = units[:n-1]
	}
	if len(units) == 0 {
		return nil
	}

	var buf []byte
	for _, u := range units {
		buf = append(buf, keyEvent(0, u, keyeventfUnicode)...)
		buf = append(buf, keyEvent(0, u, keyeventfUnicode|keyeventfKeyUp)...)
	}
	return sendInputs(buf, len(units)*2)
}

// keyCombo parses a combo like "ctrl+c", "alt+f4", "ctrl+shift+s" or a bare key
// like "enter", presses modifiers down, taps the key, then releases modifiers.
func keyCombo(combo string) error {
	ensureDPIAware()

	parts := strings.Split(strings.TrimSpace(combo), "+")
	if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
		return fmt.Errorf("empty key combo")
	}

	var mods []uint16
	var key string
	for i, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if vk, ok := modVK[p]; ok && i != len(parts)-1 {
			mods = append(mods, vk)
			continue
		}
		key = p // last (non-modifier) token is the key
	}
	if key == "" {
		return fmt.Errorf("no key in combo %q", combo)
	}

	keyVK, ok := resolveKeyVK(key)
	if !ok {
		return fmt.Errorf("unknown key %q in combo %q", key, combo)
	}

	var buf []byte
	count := 0
	// Press modifiers down (in order).
	for _, m := range mods {
		buf = append(buf, keyEvent(m, 0, 0)...)
		count++
	}
	// Key down + up.
	buf = append(buf, keyEvent(keyVK, 0, 0)...)
	buf = append(buf, keyEvent(keyVK, 0, keyeventfKeyUp)...)
	count += 2
	// Release modifiers (reverse order).
	for i := len(mods) - 1; i >= 0; i-- {
		buf = append(buf, keyEvent(mods[i], 0, keyeventfKeyUp)...)
		count++
	}
	return sendInputs(buf, count)
}

// resolveKeyVK maps a key token to a virtual-key code. Named keys come from
// vkMap; single characters fall back to VkKeyScanW for layout-aware mapping.
func resolveKeyVK(key string) (uint16, bool) {
	if vk, ok := vkMap[key]; ok {
		return vk, true
	}
	runes := []rune(key)
	if len(runes) == 1 {
		r := runes[0]
		res, _, _ := procVkKeyScanW.Call(uintptr(uint16(r)))
		low := uint16(res) & 0xFF
		if low != 0xFF {
			return low, true
		}
	}
	return 0, false
}

// scroll scrolls the mouse wheel. dy>0 scrolls up, dy<0 down; dx>0 right, dx<0
// left. Values are in "lines" (one wheelDelta per unit).
func scroll(dx, dy int) error {
	ensureDPIAware()

	var buf []byte
	count := 0
	if dy != 0 {
		buf = append(buf, mouseEvent(0, 0, uint32(int32(dy*wheelDelta)), mouseeventfWheel)...)
		count++
	}
	if dx != 0 {
		buf = append(buf, mouseEvent(0, 0, uint32(int32(dx*wheelDelta)), mouseeventfHWheel)...)
		count++
	}
	if count == 0 {
		return nil
	}
	return sendInputs(buf, count)
}
