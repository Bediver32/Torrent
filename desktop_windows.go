package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed tray_icon_windows.png
var trayIcon []byte

const (
	wmNull        = 0x0000
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmEndSession  = 0x0016
	wmCancelMode  = 0x001f
	wmContextMenu = 0x007b
	wmLeftUp      = 0x0202
	wmRightUp     = 0x0205
	wmTray        = 0x8001 // WM_APP + 1
	ninSelect     = 0x0400
	ninKeySelect  = 0x0401
	nimAdd        = 0
	nimDelete     = 2
	nimSetVersion = 4
	nifMessage    = 1
	nifIcon       = 2
	nifTip        = 4
	nifShowTip    = 0x80
	menuOpen      = 1
	menuQuit      = 2
	swShowNormal  = 1
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	kernel  = windows.NewLazySystemDLL("kernel32.dll")

	getModule         = kernel.NewProc("GetModuleHandleW")
	registerClass     = user32.NewProc("RegisterClassW")
	unregisterClass   = user32.NewProc("UnregisterClassW")
	createWindow      = user32.NewProc("CreateWindowExW")
	destroyWindow     = user32.NewProc("DestroyWindow")
	defaultWindowProc = user32.NewProc("DefWindowProcW")
	postMessage       = user32.NewProc("PostMessageW")
	postQuitMessage   = user32.NewProc("PostQuitMessage")
	getMessage        = user32.NewProc("GetMessageW")
	dispatchMessage   = user32.NewProc("DispatchMessageW")
	registerMessage   = user32.NewProc("RegisterWindowMessageW")
	createIcon        = user32.NewProc("CreateIconFromResourceEx")
	destroyIcon       = user32.NewProc("DestroyIcon")
	createMenu        = user32.NewProc("CreatePopupMenu")
	destroyMenu       = user32.NewProc("DestroyMenu")
	appendMenu        = user32.NewProc("AppendMenuW")
	trackMenu         = user32.NewProc("TrackPopupMenu")
	getCursor         = user32.NewProc("GetCursorPos")
	setForeground     = user32.NewProc("SetForegroundWindow")
	messageBox        = user32.NewProc("MessageBoxW")
	notifyIcon        = shell32.NewProc("Shell_NotifyIconW")
	shellExecute      = shell32.NewProc("ShellExecuteW")

	trayClass    = windows.StringToUTF16Ptr("PotokTrayWindow")
	trayCallback = windows.NewCallback(trayWindowProc)
	currentTray  *trayWindow // Accessed on the tray's message loop thread.
)

type winPoint struct{ X, Y int32 }

type winMessage struct {
	window  uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	point   winPoint
	private uint32
}

type winClass struct {
	style       uint32
	proc        uintptr
	classExtra  int32
	windowExtra int32
	instance    uintptr
	icon        uintptr
	cursor      uintptr
	background  uintptr
	menuName    *uint16
	className   *uint16
}

// NOTIFYICONDATAW from shellapi.h.
type trayNotification struct {
	size      uint32
	window    uintptr
	id        uint32
	flags     uint32
	callback  uint32
	icon      uintptr
	tip       [128]uint16
	state     uint32
	stateMask uint32
	info      [256]uint16
	version   uint32
	infoTitle [64]uint16
	infoFlags uint32
	guid      windows.GUID
	balloon   uintptr
}

type trayWindow struct {
	icon       trayNotification
	menu       uintptr
	taskbar    uint32
	open       func()
	stop       func()
	lastOpened time.Time
}

func startDesktop(address string, _ bool, stop chan<- struct{}) (func(), error) {
	return startTray(func() {
		if err := open(address, true); err != nil {
			showDesktopError(err)
		}
	}, func() { requestStop(stop) })
}

func startTray(openInterface, stop func()) (func(), error) {
	type result struct {
		window uintptr
		err    error
	}
	ready := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(finished)
		t, cleanup, err := createTray(openInterface, stop)
		if err != nil {
			ready <- result{err: err}
			return
		}
		defer stop()
		defer cleanup()
		ready <- result{window: t.icon.window}
		var msg winMessage
		for {
			r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				return
			}
			dispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		}
	}()
	r := <-ready
	if r.err != nil {
		return nil, r.err
	}
	return func() {
		postMessage.Call(r.window, wmCancelMode, 0, 0)
		postMessage.Call(r.window, wmClose, 0, 0)
		<-finished
	}, nil
}

func createTray(openInterface, stop func()) (_ *trayWindow, cleanup func(), err error) {
	module, _, callErr := getModule.Call(0)
	if module == 0 {
		return nil, nil, fmt.Errorf("не удалось запустить значок Потока: %w", callErr)
	}
	class := winClass{proc: trayCallback, instance: module, className: trayClass}
	registered, _, callErr := registerClass.Call(uintptr(unsafe.Pointer(&class)))
	if registered == 0 {
		return nil, nil, fmt.Errorf("не удалось зарегистрировать окно трея: %w", callErr)
	}
	t := &trayWindow{open: openInterface, stop: stop}
	release := func() {
		currentTray = nil
		if t.icon.window != 0 {
			t.icon.notify(nimDelete)
			destroyWindow.Call(t.icon.window)
		}
		if t.icon.icon != 0 {
			destroyIcon.Call(t.icon.icon)
		}
		if t.menu != 0 {
			destroyMenu.Call(t.menu)
		}
		unregisterClass.Call(uintptr(unsafe.Pointer(trayClass)), module)
	}
	defer func() {
		if err != nil {
			release()
		}
	}()
	window, _, callErr := createWindow.Call(0, uintptr(unsafe.Pointer(trayClass)),
		uintptr(unsafe.Pointer(trayClass)), 0, 0, 0, 0, 0, 0, 0, module, 0)
	if window == 0 {
		return nil, nil, fmt.Errorf("не удалось создать окно трея: %w", callErr)
	}
	t.icon = trayNotification{
		size: uint32(unsafe.Sizeof(trayNotification{})), window: window, id: 1,
		flags: nifMessage | nifIcon | nifTip | nifShowTip, callback: wmTray, version: 4,
	}
	copy(t.icon.tip[:], windows.StringToUTF16("Поток"))
	t.icon.icon, _, callErr = createIcon.Call(uintptr(unsafe.Pointer(&trayIcon[0])), uintptr(len(trayIcon)),
		1, 0x00030000, 0, 0, 0)
	if t.icon.icon == 0 {
		return nil, nil, fmt.Errorf("не удалось загрузить значок Потока: %w", callErr)
	}
	t.menu, _, callErr = createMenu.Call()
	if t.menu == 0 {
		return nil, nil, fmt.Errorf("не удалось создать меню Потока: %w", callErr)
	}
	for _, item := range []struct {
		id    uintptr
		label string
	}{{menuOpen, "Открыть Поток"}, {menuQuit, "Завершить работу"}} {
		label := windows.StringToUTF16Ptr(item.label)
		r, _, err := appendMenu.Call(t.menu, 0, item.id, uintptr(unsafe.Pointer(label)))
		if r == 0 {
			return nil, nil, fmt.Errorf("не удалось создать пункт меню Потока: %w", err)
		}
	}
	taskbar := windows.StringToUTF16Ptr("TaskbarCreated")
	msg, _, _ := registerMessage.Call(uintptr(unsafe.Pointer(taskbar)))
	t.taskbar = uint32(msg)
	currentTray = t
	if !t.addIcon() {
		return nil, nil, fmt.Errorf("не удалось добавить значок Потока в область уведомлений Windows")
	}
	return t, release, nil
}

func (n *trayNotification) notify(operation uintptr) bool {
	r, _, _ := notifyIcon.Call(operation, uintptr(unsafe.Pointer(n)))
	return r != 0
}

func (t *trayWindow) addIcon() bool {
	added := t.icon.notify(nimAdd)
	if added {
		t.icon.notify(nimSetVersion)
	}
	return added
}

func trayWindowProc(window, message, wParam, lParam uintptr) uintptr {
	t := currentTray
	if t != nil && window == t.icon.window {
		switch uint32(message) {
		case wmTray:
			switch uint16(lParam) {
			case wmLeftUp, ninSelect, ninKeySelect:
				if time.Since(t.lastOpened) >= 350*time.Millisecond {
					t.lastOpened = time.Now()
					go t.open()
				}
			case wmContextMenu, wmRightUp:
				t.showMenu()
			}
			return 0
		case t.taskbar:
			if t.taskbar != 0 {
				t.addIcon()
				return 0
			}
		case wmEndSession:
			if wParam != 0 {
				t.stop()
			}
		case wmDestroy:
			t.icon.notify(nimDelete)
			t.icon.window = 0
			postQuitMessage.Call(0)
			return 0
		}
	}
	r, _, _ := defaultWindowProc.Call(window, message, wParam, lParam)
	return r
}

func (t *trayWindow) showMenu() {
	var point winPoint
	r, _, _ := getCursor.Call(uintptr(unsafe.Pointer(&point)))
	if r == 0 {
		return
	}
	setForeground.Call(t.icon.window)
	const tpmReturnCmd = 0x100
	const tpmRightButton = 0x2
	command, _, _ := trackMenu.Call(t.menu, tpmReturnCmd|tpmRightButton,
		uintptr(point.X), uintptr(point.Y), 0, t.icon.window, 0)
	postMessage.Call(t.icon.window, wmNull, 0, 0)
	switch command {
	case menuOpen:
		go t.open()
	case menuQuit:
		t.stop()
	}
}

func open(target string, _ bool) error {
	path, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	r, _, _ := shellExecute.Call(0, 0, uintptr(unsafe.Pointer(path)), 0, 0, swShowNormal)
	if r <= 32 {
		return fmt.Errorf("Windows не смогла открыть %s (код %d)", target, r)
	}
	return nil
}

func showDesktopError(err error) {
	text := windows.StringToUTF16Ptr(err.Error())
	title := windows.StringToUTF16Ptr("Поток")
	messageBox.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10)
}
