//go:build linux && tray

package desktop

/*
#cgo pkg-config: ayatana-appindicator3-0.1 gtk+-3.0
#include <stdlib.h>
#include <gtk/gtk.h>
#include <libayatana-appindicator/app-indicator.h>

void monloader_tray_connect(GtkWidget *item, int which);
*/
import "C"

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// The Linux tray is behind a build tag because it needs CGo and
// libayatana-appindicator, and on GNOME the icon does not appear at all
// without a user-installed extension. No shipped artifact sets that tag - the
// archives, the AppImage and the Flatpak all build CGO_ENABLED=0 - so this is
// `make build-tray` alone, which CI compiles so it cannot rot unseen. It is
// never the only route to anything.

// trayState is the running menu. GTK owns the thread, and the callbacks
// come back from C with no user data, so the state is package-level and
// guarded rather than threaded through.
var trayState struct {
	mu        sync.Mutex
	menu      TrayMenu
	autostart *C.GtkCheckMenuItem
	suppress  bool // set while the code, not the user, moves the tick
}

//export monloaderTrayOpen
func monloaderTrayOpen() {
	trayState.mu.Lock()
	open := trayState.menu.Open
	trayState.mu.Unlock()
	if open != nil {
		go open()
	}
}

//export monloaderTrayQuit
func monloaderTrayQuit() {
	trayState.mu.Lock()
	quit := trayState.menu.Quit
	trayState.mu.Unlock()
	C.gtk_main_quit()
	if quit != nil {
		go quit()
	}
}

//export monloaderTrayAutostart
func monloaderTrayAutostart() {
	trayState.mu.Lock()
	defer trayState.mu.Unlock()
	if trayState.suppress {
		return
	}
	on := trayState.menu.toggleAutostart()
	// Writing the file can fail; the tick has to follow the disk, not the
	// click, and setting it back re-enters this callback.
	trayState.suppress = true
	C.gtk_check_menu_item_set_active(trayState.autostart, cbool(on))
	trayState.suppress = false
}

func cbool(b bool) C.gboolean {
	if b {
		return C.TRUE
	}
	return C.FALSE
}

// TrayAvailable reports whether this build has a tray at all, which is what
// decides whether Settings offers a switch for it.
func TrayAvailable() bool { return true }

// RunTray serves the tray until ctx is done. GTK's main loop owns the
// thread it is started on, so the goroutine is locked to one.
func RunTray(ctx context.Context, m TrayMenu) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if C.gtk_init_check(nil, nil) == C.FALSE {
		return fmt.Errorf("no display for the tray: %w", ErrTrayUnavailable)
	}

	trayState.mu.Lock()
	trayState.menu = m
	trayState.mu.Unlock()

	id := C.CString(m.Autostart.App)
	defer C.free(unsafe.Pointer(id))
	icon := C.CString(m.Autostart.App)
	defer C.free(unsafe.Pointer(icon))
	title := C.CString(m.Title)
	defer C.free(unsafe.Pointer(title))

	indicator := C.app_indicator_new(id, icon, C.APP_INDICATOR_CATEGORY_APPLICATION_STATUS)
	if indicator == nil {
		return ErrTrayUnavailable
	}
	C.app_indicator_set_title(indicator, title)
	C.app_indicator_set_status(indicator, C.APP_INDICATOR_STATUS_ACTIVE)

	menu := C.gtk_menu_new()
	appendTrayItem(menu, "Open", 0)
	if show, on := m.autostartItem(); show {
		label := C.CString("Start at login")
		item := C.gtk_check_menu_item_new_with_label(label)
		C.free(unsafe.Pointer(label))
		trayState.autostart = (*C.GtkCheckMenuItem)(unsafe.Pointer(item))
		C.gtk_check_menu_item_set_active(trayState.autostart, cbool(on))
		C.gtk_menu_shell_append((*C.GtkMenuShell)(unsafe.Pointer(menu)), item)
		C.gtk_widget_show(item)
		C.monloader_tray_connect(item, 1)
	}
	appendTrayItem(menu, "Quit", 2)
	C.app_indicator_set_menu(indicator, (*C.GtkMenu)(unsafe.Pointer(menu)))

	// Shutting the app down has to reach GTK's loop, which only leaves it
	// on its own quit call.
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			C.gtk_main_quit()
		case <-stopped:
		}
	}()
	C.gtk_main()
	close(stopped)
	return nil
}

func appendTrayItem(menu *C.GtkWidget, label string, which C.int) {
	text := C.CString(label)
	defer C.free(unsafe.Pointer(text))
	item := C.gtk_menu_item_new_with_label(text)
	C.gtk_menu_shell_append((*C.GtkMenuShell)(unsafe.Pointer(menu)), item)
	C.gtk_widget_show(item)
	C.monloader_tray_connect(item, which)
}
