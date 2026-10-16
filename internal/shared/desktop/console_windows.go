package desktop

import (
	"log"
	"os"

	"golang.org/x/sys/windows"
)

// ATTACH_PARENT_PROCESS, (DWORD)-1.
const attachParentProcess = uintptr(0xFFFFFFFF)

// AttachConsole failing from a shortcut, which has no parent console, is
// the wanted no-op.
func AttachConsole() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	if kernel32.Load() != nil {
		return
	}
	if r, _, _ := kernel32.NewProc("AttachConsole").Call(attachParentProcess); r == 0 {
		return
	}
	// A GUI launch was handed invalid standard handles; only CONOUT$,
	// opened by name, reaches the console.
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	os.Stdout, os.Stderr = out, out
	// The default logger captured the old os.Stderr at init.
	log.SetOutput(out)
	if in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = in
	}
}
