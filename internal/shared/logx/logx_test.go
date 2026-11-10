package logx

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestEnabledMatchesThreshold(t *testing.T) {
	for _, tc := range []struct {
		threshold         string
		debug, info, warn bool
	}{
		{"debug", true, true, true},
		{"info", false, true, true},
		{"warn", false, false, true},
		{"bogus", false, false, true},
	} {
		Set(tc.threshold)
		for l, want := range map[Level]bool{LevelDebug: tc.debug, LevelInfo: tc.info, LevelWarn: tc.warn} {
			if got := Enabled(l); got != want {
				t.Errorf("threshold=%q level=%d: Enabled = %v, want %v", tc.threshold, l, got, want)
			}
		}
	}
}

func TestGetNamesTheLevelSetTook(t *testing.T) {
	for in, want := range map[string]string{"debug": "debug", " INFO ": "info", "warn": "warn", "bogus": "warn"} {
		Set(in)
		if got := Get(); got != want {
			t.Errorf("Set(%q): Get = %q, want %q", in, got, want)
		}
	}
	Set("warn")
}

func captureOutput(fn func()) string {
	var buf bytes.Buffer
	saved := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(saved)
	fn()
	return buf.String()
}

func TestWarnLevelSuppressesInfoAndDebug(t *testing.T) {
	Set("warn")
	out := captureOutput(func() {
		Debugf("dbg-%d", 1)
		Infof("inf-%d", 2)
		Warnf("wrn-%d", 3)
		Errorf("err-%d", 4)
	})
	if strings.Contains(out, "dbg-1") {
		t.Errorf("warn level should suppress Debugf, got: %q", out)
	}
	if strings.Contains(out, "inf-2") {
		t.Errorf("warn level should suppress Infof, got: %q", out)
	}
	if !strings.Contains(out, "WARN wrn-3") {
		t.Errorf("Warnf should always fire, got: %q", out)
	}
	if !strings.Contains(out, "ERROR err-4") {
		t.Errorf("Errorf should always fire, got: %q", out)
	}
}

func TestDebugLevelEmitsEverything(t *testing.T) {
	Set("debug")
	out := captureOutput(func() {
		Debugf("dbg-%d", 1)
		Infof("inf-%d", 2)
		Warnf("wrn-%d", 3)
	})
	for _, want := range []string{"DEBUG dbg-1", "INFO inf-2", "WARN wrn-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("debug level should emit %q, got: %q", want, out)
		}
	}
}
