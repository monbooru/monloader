//go:build linux

package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testHook(t *testing.T) Hook {
	t.Helper()
	xdgHome(t)
	t.Setenv("FLATPAK_ID", "")
	return Hook{App: "monbooru", Name: "Monbooru", Comment: "Lightweight and fast private booru", Categories: "Graphics;Viewer;", Icon: onePixelPNG}
}

var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
	0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0, 0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 0x0a, 'I', 'D', 'A', 'T', 0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1,
	0x0d, 0x0a, 0x2d, 0xb4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

func TestMenuEntryRoundTrip(t *testing.T) {
	h := testHook(t)
	if !h.MenuSupported() {
		t.Skip("this build is installed under a system prefix")
	}
	if h.MenuEnabled() {
		t.Fatal("the entry should not exist yet")
	}
	if err := h.SetMenu(true); err != nil {
		t.Fatalf("SetMenu(true): %v", err)
	}
	if !h.MenuEnabled() {
		t.Fatal("the entry did not land")
	}
	path, err := menuEntryPath(h.App)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Type=Application", "Name=Monbooru", "-desktop", "Icon=monbooru", "Categories=Graphics;Viewer;\n"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("entry is missing %q:\n%s", want, body)
		}
	}
	share, _ := shareRoot()
	if _, err := os.Stat(filepath.Join(share, "icons", "hicolor", "1x1", "apps", "monbooru.png")); err != nil {
		t.Errorf("icon not installed: %v", err)
	}

	if err := h.SetMenu(false); err != nil {
		t.Fatalf("SetMenu(false): %v", err)
	}
	if h.MenuEnabled() {
		t.Error("the entry survived removal")
	}
}

func TestAutostartRoundTrip(t *testing.T) {
	h := testHook(t)
	if h.AutostartEnabled() {
		t.Fatal("autostart should not be on yet")
	}
	if err := h.SetAutostart(true); err != nil {
		t.Fatalf("SetAutostart(true): %v", err)
	}
	path, err := autostartPath(h.App)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "X-GNOME-Autostart-enabled=true") {
		t.Errorf("autostart entry is missing the enable key:\n%s", body)
	}
	if err := h.SetAutostart(false); err != nil {
		t.Fatalf("SetAutostart(false): %v", err)
	}
	if h.AutostartEnabled() {
		t.Error("autostart survived removal")
	}
}

func TestSandboxedIntegrationTargetsTheHost(t *testing.T) {
	h := testHook(t)
	t.Setenv("FLATPAK_ID", "io.github.monbooru.Monbooru")
	if h.MenuSupported() {
		t.Error("a sandboxed install already ships a menu entry")
	}
	if cmd := LaunchCommand(); cmd != "flatpak run io.github.monbooru.Monbooru -desktop" {
		t.Errorf("LaunchCommand() = %q", cmd)
	}
	path, err := autostartPath(h.App)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, ".config", "autostart", "monbooru.desktop"); path != want {
		t.Errorf("autostart path = %q, want %q", path, want)
	}
}

func TestToggleAutostartFollowsTheDisk(t *testing.T) {
	xdgHome(t)
	t.Setenv("FLATPAK_ID", "")
	m := TrayMenu{Autostart: Hook{App: "monbooru", Name: "Monbooru"}}

	show, on := m.autostartItem()
	if !show || on {
		t.Fatalf("initial state: show=%v on=%v", show, on)
	}
	if got := m.toggleAutostart(); !got {
		t.Fatal("toggle did not turn it on")
	}
	path, err := autostartPath("monbooru")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("no autostart entry written: %v", err)
	}
	if got := m.toggleAutostart(); got {
		t.Error("toggle did not turn it off")
	}
}

func TestApplyWritesBothEntriesAndTakesThemBack(t *testing.T) {
	h := testHook(t)
	if !h.MenuSupported() || !h.AutostartSupported() {
		t.Skip("this build is installed under a system prefix")
	}
	if f := h.Apply(true, true); len(f) != 0 {
		t.Fatalf("Apply(true, true) failed: %v", f)
	}
	if !h.MenuEnabled() || !h.AutostartEnabled() {
		t.Fatal("an entry did not land")
	}
	if f := h.Apply(false, false); len(f) != 0 || h.MenuEnabled() || h.AutostartEnabled() {
		t.Errorf("Apply(false, false) = %v, menu %v, autostart %v", f, h.MenuEnabled(), h.AutostartEnabled())
	}
}
