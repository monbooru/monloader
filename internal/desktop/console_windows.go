package desktop

import (
	"log"
	"os"

	"golang.org/x/sys/windows"
)

// attachParentProcess is AttachConsole's (DWORD)-1: the console of the
// process that launched this one.
const attachParentProcess = uintptr(0xFFFFFFFF)

// AttachConsole hands a GUI-subsystem binary the console it was started
// from. Windows gives it none, so -version, the password hash and every
// startup line would otherwise be written to handles that go nowhere. A
// launch from a shortcut has no parent console and the call fails, which
// is the no-op that case wants. Windows does not wait on a GUI-subsystem
// process either, so the shell prompt comes back before the output lands.
func AttachConsole() {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	if kernel32.Load() != nil {
		return
	}
	if r, _, _ := kernel32.NewProc("AttachConsole").Call(attachParentProcess); r == 0 {
		return
	}
	// The standard handles a GUI launch was given are the invalid ones; the
	// console has to be opened by name for anything to reach it.
	out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	os.Stdout, os.Stderr = out, out
	// The default logger captured the old os.Stderr at init, and the desktop
	// profile's log file is layered on top of whatever it holds now.
	log.SetOutput(out)
	if in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = in
	}
}
