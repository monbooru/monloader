//go:build !windows

package gdl

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/monbooru/monloader/internal/config"
)

func processGone(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] == "Z"
}

func TestCancelKillsTheChildOfAOneFileBuild(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc here")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	_, cfg := stubGDL(t, dir, "#!/bin/sh\nsleep 30 &\necho $! > "+pidFile+"\nwait\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = New(config.NewProvider(cfg), SiteLists{}).Download(ctx, "https://example.com/x", "", filepath.Join(dir, "work"), ArchiveRead, nil, false)
		close(done)
	}()
	pid := childPID(t, pidFile)
	cancel()
	<-done
	waitGone(t, pid)
}

func TestCancelKillsTheChildOfAVersionProbe(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc here")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	stub, _ := stubGDL(t, dir, "#!/bin/sh\nsleep 30 >/dev/null 2>&1 &\necho $! > "+pidFile+"\nwait\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		BinaryVersion(ctx, stub)
		close(done)
	}()
	pid := childPID(t, pidFile)
	cancel()
	<-done
	waitGone(t, pid)
}

func childPID(t *testing.T, pidFile string) int {
	t.Helper()
	var pid int
	for deadline := time.Now().Add(2 * time.Second); pid == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the stub never started its child")
		}
		data, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		time.Sleep(5 * time.Millisecond)
	}
	return pid
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !processGone(pid); {
		if time.Now().After(deadline) {
			t.Fatalf("the child %d outlived the cancel", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
