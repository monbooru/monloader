package desktop

import (
	"runtime"
	"testing"
)

func TestOpenerPerPlatform(t *testing.T) {
	want := map[string]string{"windows": "explorer", "darwin": "open"}[runtime.GOOS]
	if want == "" {
		want = "xdg-open"
	}
	if name := opener(); name != want {
		t.Errorf("opener = %q, want %q on %s", name, want, runtime.GOOS)
	}
}

func TestOpenReportsAMissingOpener(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for name, open := range map[string]func(string) error{
		"OpenBrowser": OpenBrowser,
		"OpenFolder":  OpenFolder,
	} {
		if err := open("/nowhere"); err == nil {
			t.Errorf("%s should report an opener that is not on PATH", name)
		}
	}
}
