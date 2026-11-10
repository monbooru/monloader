package desktop

import "testing"

func TestExecQuote(t *testing.T) {
	cases := map[string]string{
		"/usr/bin/monbooru":         "/usr/bin/monbooru",
		"/home/me/My Apps/monbooru": `"/home/me/My Apps/monbooru"`,
		`/opt/a"b/monbooru`:         `"/opt/a\\"b/monbooru"`,
		`/opt/a\b/monbooru`:         `"/opt/a\\\\b/monbooru"`,
		"/opt/a(1)/monbooru":        `"/opt/a(1)/monbooru"`,
		"/opt/50%/monbooru":         "/opt/50%%/monbooru",
		"/opt/My 50%/monbooru":      `"/opt/My 50%%/monbooru"`,
	}
	for in, want := range cases {
		if got := execQuote(in); got != want {
			t.Errorf("execQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLaunchCommandUnderFlatpak(t *testing.T) {
	t.Setenv("FLATPAK_ID", "io.github.monbooru.Monbooru")
	if got, want := LaunchCommand(), "flatpak run io.github.monbooru.Monbooru -desktop"; got != want {
		t.Errorf("LaunchCommand = %q, want %q", got, want)
	}
}

func TestLaunchCommandUnderAppImage(t *testing.T) {
	t.Setenv("FLATPAK_ID", "")
	t.Setenv("APPIMAGE", "/home/me/Apps/Monbooru.AppImage")
	if got, want := LaunchCommand(), "/home/me/Apps/Monbooru.AppImage -desktop"; got != want {
		t.Errorf("LaunchCommand = %q, want %q", got, want)
	}
}
