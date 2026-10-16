//go:build linux && tray

package desktop

/*
#cgo pkg-config: ayatana-appindicator3-0.1 gtk+-3.0
#include <stdlib.h>
#include <gtk/gtk.h>
#include <libayatana-appindicator/app-indicator.h>

void desktop_tray_connect(GtkWidget *item, int which);
*/
import "C"

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Behind the tray tag: it needs CGo and libayatana-appindicator. No
// shipped artifact sets the tag.

// Package-level: the C callbacks carry no user data.
var trayState struct {
	mu        sync.Mutex
	menu      TrayMenu
	autostart *C.GtkCheckMenuItem
	suppress  bool
}

//export desktopTrayOpen
func desktopTrayOpen() {
	trayState.mu.Lock()
	open := trayState.menu.Open
	trayState.mu.Unlock()
	if open != nil {
		go open()
	}
}

//export desktopTrayQuit
func desktopTrayQuit() {
	trayState.mu.Lock()
	quit := trayState.menu.Quit
	trayState.mu.Unlock()
	C.gtk_main_quit()
	if quit != nil {
		go quit()
	}
}

//export desktopTrayAutostart
func desktopTrayAutostart() {
	if trayState.suppress {
		return
	}
	trayState.mu.Lock()
	menu := trayState.menu
	trayState.mu.Unlock()
	on := menu.toggleAutostart()
	// The tick follows the disk, not the click, and setting it re-enters
	// this callback on this thread, so mu must not be held here.
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

func TrayAvailable() bool { return true }

// RunTray locks its goroutine to one thread: GTK's main loop owns the
// thread it starts on.
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
		C.desktop_tray_connect(item, 1)
	}
	appendTrayItem(menu, "Quit", 2)
	C.app_indicator_set_menu(indicator, (*C.GtkMenu)(unsafe.Pointer(menu)))

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
	C.desktop_tray_connect(item, which)
}
