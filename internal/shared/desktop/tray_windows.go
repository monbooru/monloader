package desktop

import (
	"context"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Not behind the tray tag: no CGo here, and a GUI-subsystem binary
// otherwise shows no sign of running.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassEx   = user32.NewProc("RegisterClassExW")
	procCreateWindowEx    = user32.NewProc("CreateWindowExW")
	procDefWindowProc     = user32.NewProc("DefWindowProcW")
	procDestroyWindow     = user32.NewProc("DestroyWindow")
	procGetMessage        = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessage   = user32.NewProc("DispatchMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procPostMessage       = user32.NewProc("PostMessageW")
	procCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	procAppendMenu        = user32.NewProc("AppendMenuW")
	procDestroyMenu       = user32.NewProc("DestroyMenu")
	procTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	procGetCursorPos      = user32.NewProc("GetCursorPos")
	procSetForegroundWin  = user32.NewProc("SetForegroundWindow")
	procLoadImage         = user32.NewProc("LoadImageW")
	procLoadIcon          = user32.NewProc("LoadIconW")
	procRegisterWindowMsg = user32.NewProc("RegisterWindowMessageW")
	procShellNotifyIcon   = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandle   = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmDestroy      = 0x0002
	wmClose        = 0x0010
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmTrayCallback = 0x0400 + 1 // WM_APP + 1

	nimAdd     = 0x0000
	nimDelete  = 0x0002
	nifMessage = 0x0001
	nifIcon    = 0x0002
	nifTip     = 0x0004

	mfString  = 0x0000
	mfChecked = 0x0008

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	imageIcon      = 1
	lrLoadFromFile = 0x0010
	lrDefaultSize  = 0x0040

	idiApplication = 32512

	menuOpen      = 1
	menuAutostart = 2
	menuQuit      = 3
)

type point struct{ X, Y int32 }

type msgStruct struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             windows.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

func TrayAvailable() bool { return true }

// RunTray locks its goroutine to one thread: the message pump must own it
// for the window's whole life.
func RunTray(ctx context.Context, m TrayMenu) error {
	// Call panics if the DLL cannot load, as on a session with no window
	// station, and that takes the process down.
	for _, p := range []*windows.LazyProc{procRegisterClassEx, procShellNotifyIcon} {
		if err := p.Find(); err != nil {
			return fmt.Errorf("tray unavailable: %w", err)
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	instance, _, _ := procGetModuleHandle.Call(0)
	className, err := windows.UTF16PtrFromString("MonTrayWindow")
	if err != nil {
		return err
	}
	// Broadcast when the taskbar comes up; an explorer restart drops
	// every icon.
	taskbarCreatedName, err := windows.UTF16PtrFromString("TaskbarCreated")
	if err != nil {
		return err
	}
	taskbarCreated, _, _ := procRegisterWindowMsg.Call(uintptr(unsafe.Pointer(taskbarCreatedName)))

	var hwnd windows.Handle
	var nid notifyIconData
	wndProc := windows.NewCallback(func(h windows.Handle, message uint32, wparam, lparam uintptr) uintptr {
		switch message {
		case wmTrayCallback:
			switch uint32(lparam) {
			case wmLButtonUp:
				if m.Open != nil {
					go m.Open()
				}
			case wmRButtonUp:
				showTrayMenu(h, m)
			}
			return 0
		case wmClose:
			// Our only window: an outside close, such as the installer's
			// restart manager, quits the app, not just the icon.
			if m.Quit != nil {
				go m.Quit()
			}
			procDestroyWindow.Call(uintptr(h)) //nolint:errcheck
			return 0
		case wmDestroy:
			procPostQuitMessage.Call(0) //nolint:errcheck
			return 0
		case uint32(taskbarCreated):
			procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid))) //nolint:errcheck
			return 0
		}
		ret, _, _ := procDefWindowProc.Call(uintptr(h), uintptr(message), wparam, lparam)
		return ret
	})

	class := wndClassEx{
		Style:         0,
		LpfnWndProc:   wndProc,
		HInstance:     windows.Handle(instance),
		LpszClassName: className,
	}
	class.CbSize = uint32(unsafe.Sizeof(class))
	if atom, _, callErr := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("register tray window class: %w", callErr)
	}

	// Top-level, not HWND_MESSAGE: a message-only window never gets the
	// TaskbarCreated broadcast.
	h, _, callErr := procCreateWindowEx.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0, 0, instance, 0,
	)
	if h == 0 {
		return fmt.Errorf("create tray window: %w", callErr)
	}
	hwnd = windows.Handle(h)

	nid = notifyIconData{
		HWnd:             hwnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTrayCallback,
		HIcon:            trayIcon(m.IconPath),
	}
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	copyUTF16(nid.SzTip[:], m.Title)
	// May fail at login before the taskbar exists; TaskbarCreated re-adds it.
	procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))          //nolint:errcheck
	defer procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid))) //nolint:errcheck

	go func() {
		<-ctx.Done()
		procPostMessage.Call(uintptr(hwnd), wmClose, 0, 0) //nolint:errcheck
	}()

	var message msgStruct
	for {
		ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(ret) <= 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&message))) //nolint:errcheck
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))  //nolint:errcheck
	}
}

// Built per right-click so the start-at-login tick matches the disk.
func showTrayMenu(hwnd windows.Handle, m TrayMenu) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu) //nolint:errcheck

	appendItem(menu, mfString, menuOpen, "Open")
	if show, on := m.autostartItem(); show {
		flags := uintptr(mfString)
		if on {
			flags |= mfChecked
		}
		appendItem(menu, flags, menuAutostart, "Start at login")
	}
	appendItem(menu, mfString, menuQuit, "Quit")

	// Without foreground ownership the menu does not close on a click
	// elsewhere.
	procSetForegroundWin.Call(uintptr(hwnd)) //nolint:errcheck
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))) //nolint:errcheck
	cmd, _, _ := procTrackPopupMenu.Call(menu,
		tpmLeftAlign|tpmRightButton|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)

	switch cmd {
	case menuOpen:
		if m.Open != nil {
			go m.Open()
		}
	case menuAutostart:
		go m.toggleAutostart()
	case menuQuit:
		if m.Quit != nil {
			go m.Quit()
		}
	}
}

func appendItem(menu, flags, id uintptr, text string) {
	p, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procAppendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(p))) //nolint:errcheck
}

func trayIcon(path string) windows.Handle {
	if path != "" {
		if p, err := windows.UTF16PtrFromString(path); err == nil {
			h, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(p)),
				imageIcon, 0, 0, lrLoadFromFile|lrDefaultSize)
			if h != 0 {
				return windows.Handle(h)
			}
		}
	}
	h, _, _ := procLoadIcon.Call(0, idiApplication)
	return windows.Handle(h)
}

func copyUTF16(dst []uint16, s string) {
	encoded := windows.StringToUTF16(s)
	if len(encoded) > len(dst) {
		encoded = encoded[:len(dst)]
		encoded[len(encoded)-1] = 0
	}
	copy(dst, encoded)
}
