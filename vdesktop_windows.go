//go:build windows

package main

import (
	"runtime"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

// Virtual-desktop awareness for the screen MCP.
//
// Windows keeps windows from other virtual desktops enumerable (EnumWindows lists
// them all), but marks them DWM-cloaked. Capturing or coordinate-clicking a
// cloaked window hits whatever is on the CURRENT desktop at those pixels, not the
// window — so we must switch to a window's desktop before operating on it, and be
// able to switch back afterwards.
//
// "Is this window on another desktop?" uses DwmGetWindowAttribute (plain Win32,
// no COM). Picking a reliable return anchor uses the stable, public
// IVirtualDesktopManager COM interface (NOT the unstable internal one).

var (
	modDwmVD = windows.NewLazySystemDLL("dwmapi.dll")

	procDwmGetWindowAttribute = modDwmVD.NewProc("DwmGetWindowAttribute")
)

const (
	dwmwaCloaked      = 14  // DWMWA_CLOAKED
	dwmCloakedShell   = 0x2 // DWM_CLOAKED_SHELL — cloaked by the shell for a virtual desktop
	spiSetFgLock      = 0x2001
	spifSendChange    = 0x0002
	lsfwUnlock        = 2 // LockSetForegroundWindow(LSFW_UNLOCK)
	coInitApartmented = 0x2
)

// CLSID_VirtualDesktopManager / IID_IVirtualDesktopManager (public, stable).
var clsidVirtualDesktopManager = ole.NewGUID("{aa509086-5ca9-4c25-8f95-589d3c07b48a}")
var iidIVirtualDesktopManager = ole.NewGUID("{a5cd92ff-29be-454c-8d04-d82879fb3f1b}")

// IVirtualDesktopManager vtable slots (after IUnknown 0,1,2).
const (
	vdmIsWindowOnCurrentVirtualDesktop = 3 // (HWND, *BOOL)
	vdmGetWindowDesktopId              = 4 // (HWND, *GUID)
)

// isWindowOnAnotherDesktop reports whether hwnd is on a virtual desktop other
// than the active one, detected via the DWM shell-cloak flag (no COM needed).
func isWindowOnAnotherDesktop(hwnd uintptr) bool {
	var flags uint32
	r, _, _ := procDwmGetWindowAttribute.Call(hwnd, dwmwaCloaked,
		uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags))
	if int32(r) < 0 { // DwmGetWindowAttribute failed
		return false
	}
	return flags&dwmCloakedShell != 0
}

// vdmDo runs fn with a live IVirtualDesktopManager on a COM-initialized, OS-locked
// thread. Best-effort: if COM/instance creation fails, fn is not called.
func vdmDo(fn func(vdm *ole.IUnknown)) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ole.CoInitializeEx(0, coInitApartmented); err == nil {
		defer ole.CoUninitialize()
	}
	unk, err := ole.CreateInstance(clsidVirtualDesktopManager, iidIVirtualDesktopManager)
	if err != nil || unk == nil {
		return
	}
	defer unk.Release()
	fn(unk)
}

func vdmCall(this *ole.IUnknown, slot int, args ...uintptr) uintptr {
	vtbl := (*[64]uintptr)(unsafe.Pointer(this.RawVTable))
	all := append([]uintptr{uintptr(unsafe.Pointer(this))}, args...)
	ret, _, _ := syscall.SyscallN(vtbl[slot], all...)
	return ret
}

// pickReturnAnchor returns a visible, titled, NON-pinned window that is on the
// CURRENT virtual desktop — a reliable handle to re-focus later to switch back to
// this desktop. Pinned/all-desktop windows (desktop GUID == NULL) are skipped
// because focusing them never forces a desktop switch. Returns 0 if none found.
func pickReturnAnchor() uintptr {
	var anchor uintptr
	vdmDo(func(vdm *ole.IUnknown) {
		cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
			if anchor != 0 {
				return 1
			}
			if v, _, _ := procIsWindowVisible.Call(h); v == 0 {
				return 1
			}
			if getWindowText(h) == "" {
				return 1
			}
			var onCur int32
			vdmCall(vdm, vdmIsWindowOnCurrentVirtualDesktop, h, uintptr(unsafe.Pointer(&onCur)))
			if onCur == 0 {
				return 1
			}
			var gid ole.GUID
			vdmCall(vdm, vdmGetWindowDesktopId, h, uintptr(unsafe.Pointer(&gid)))
			if gid.Data1 == 0 && gid.Data2 == 0 && gid.Data3 == 0 {
				return 1 // pinned to all desktops — can't anchor a switch
			}
			anchor = h
			return 1
		})
		procEnumWindows.Call(cb, 0)
	})
	return anchor
}
