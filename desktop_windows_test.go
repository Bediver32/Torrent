package main

import (
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestTrayClick(t *testing.T) {
	findWindow := user32.NewProc("FindWindowW")
	explorer := windows.StringToUTF16Ptr("Shell_TrayWnd")
	shell, _, _ := findWindow.Call(uintptr(unsafe.Pointer(explorer)), 0)
	if shell == 0 {
		t.Skip("the desktop session has no Windows notification area")
	}
	opened := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	cleanup, err := startTray(func() { requestStop(opened) }, func() { requestStop(done) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	window, _, _ := findWindow.Call(uintptr(unsafe.Pointer(trayClass)), 0)
	if window == 0 {
		t.Fatal("tray window not found")
	}
	if result, _, err := postMessage.Call(window, wmTray, 0, wmLeftUp); result == 0 {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-done:
		t.Fatal("tray exited before processing the click")
	case <-time.After(5 * time.Second):
		t.Fatal("tray click did not open the interface")
	}
}
