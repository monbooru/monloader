package desktop

import (
	"strings"
	"testing"
)

func TestRunValueQuotesForTheWindowsShell(t *testing.T) {
	got := runValue()
	if got == "" {
		t.Fatal("runValue() is empty")
	}
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `" -desktop`) {
		t.Errorf("runValue() = %q, want the path in plain double quotes", got)
	}
	if strings.Contains(got, `\"`) || strings.Contains(got, `\$`) || strings.Contains(got, "\\`") {
		t.Errorf("runValue() = %q carries freedesktop Exec escaping", got)
	}
}
