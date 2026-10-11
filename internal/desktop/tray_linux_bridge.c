//go:build linux && tray

// Not in the Go preamble: beside //export it is copied into two translation
// units, so it may hold declarations only.

#include <gtk/gtk.h>
#include "_cgo_export.h"

static void monloader_tray_on_open(GtkMenuItem *item, gpointer data) {
	(void)item; (void)data;
	monloaderTrayOpen();
}

static void monloader_tray_on_autostart(GtkMenuItem *item, gpointer data) {
	(void)item; (void)data;
	monloaderTrayAutostart();
}

static void monloader_tray_on_quit(GtkMenuItem *item, gpointer data) {
	(void)item; (void)data;
	monloaderTrayQuit();
}

void monloader_tray_connect(GtkWidget *item, int which) {
	switch (which) {
	case 0:
		g_signal_connect(item, "activate", G_CALLBACK(monloader_tray_on_open), NULL);
		break;
	case 1:
		g_signal_connect(item, "activate", G_CALLBACK(monloader_tray_on_autostart), NULL);
		break;
	default:
		g_signal_connect(item, "activate", G_CALLBACK(monloader_tray_on_quit), NULL);
		break;
	}
}
