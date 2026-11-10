package desktop

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestRunTrayReportsWhenThereIsNone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows build always has a tray")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunTray(ctx, TrayMenu{Title: "monbooru"})
	if !errors.Is(err, ErrTrayUnavailable) {
		t.Errorf("RunTray = %v, want ErrTrayUnavailable", err)
	}
}

func TestAutostartItemHidesWhenUnsupported(t *testing.T) {
	if show, _ := (TrayMenu{}).autostartItem(); show {
		t.Error("an unnamed app should not get a start-at-login entry")
	}
}
