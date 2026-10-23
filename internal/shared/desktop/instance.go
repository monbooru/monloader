package desktop

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"
)

var ErrRunning = errors.New("already running")

// A loopback request to a process that answers at once or is not there.
const probeTimeout = 500 * time.Millisecond

// Two launches can both find the port empty; the one that loses the bind
// waits up to wait for the winner to answer.
func Listen(app, addr string, wait time.Duration) (net.Listener, error) {
	local := LoopbackAddr(addr)
	if inst, found := Probe(local, probeTimeout); found {
		return nil, occupied(app, inst, local)
	}
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, nil
	}
	if addrInUse(err) {
		for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
			if inst, found := Probe(local, probeTimeout); found {
				return nil, occupied(app, inst, local)
			}
		}
	}
	return nil, fmt.Errorf("could not listen on %s: %w; free the port or set server.bind_address to another one", addr, err)
}

func occupied(app string, inst Instance, local string) error {
	if inst.App == app {
		return ErrRunning
	}
	return fmt.Errorf("%s is already serving %s; free the port or set server.bind_address to another one", cmp.Or(inst.App, "another program"), local)
}

func HandOver(app, addr string, openBrowser bool) error {
	local := LoopbackAddr(addr)
	fmt.Printf("%s is already running on http://%s\n", app, local)
	if !openBrowser {
		return nil
	}
	return openLocal(local)
}

func OpenWhenServing(app, addr string, wait time.Duration) error {
	local := LoopbackAddr(addr)
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if inst, found := Probe(local, probeTimeout); found && inst.App == app {
			return openLocal(local)
		}
	}
	return fmt.Errorf("the server did not answer in time; open http://%s yourself", local)
}

func openLocal(local string) error {
	if err := OpenBrowser("http://" + local); err != nil {
		return fmt.Errorf("could not open a browser: %w", err)
	}
	return nil
}

// The relaunch adds -no-browser: the page that asked for the restart reloads
// itself.
func Relaunch() error {
	exe := Program()
	if exe == "" {
		return errors.New("locating this executable")
	}
	args := os.Args[1:]
	if !slices.Contains(args, "-no-browser") {
		args = append(slices.Clone(args), "-no-browser")
	}
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func TrayIcon(app string) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	dir := ExeDir()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, app+".ico")
	if !IsFile(p) {
		return ""
	}
	return p
}
