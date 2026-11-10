package desktop

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestListenBindsAFreePort(t *testing.T) {
	ln, err := Listen("app", freeAddr(t), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
}

func TestListenHandsOverToTheRunningCopy(t *testing.T) {
	if _, err := Listen("app", probeStub(t, http.StatusOK, `{"app":"app","status":"ok"}`), time.Second); !errors.Is(err, ErrRunning) {
		t.Errorf("err = %v, want ErrRunning", err)
	}
}

func TestListenNamesAStrangerOnThePort(t *testing.T) {
	_, err := Listen("app", probeStub(t, http.StatusOK, `{"app":"caddy","status":"ok"}`), time.Second)
	if err == nil || errors.Is(err, ErrRunning) || !strings.Contains(err.Error(), "caddy") {
		t.Errorf("err = %v, want the occupant named", err)
	}
}

func TestALostBindWaitsForTheWinnerToAnswer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"app":"app","status":"ok"}`))
	})}

	time.AfterFunc(600*time.Millisecond, func() { _ = srv.Serve(ln) })
	defer srv.Close()
	if _, err := Listen("app", ln.Addr().String(), 10*time.Second); !errors.Is(err, ErrRunning) {
		t.Errorf("err = %v, want ErrRunning once the winner answers", err)
	}
}

func TestAnotherBindErrorIsNotWaitedOn(t *testing.T) {
	start := time.Now()
	_, err := Listen("app", "203.0.113.1:1", 30*time.Second)
	if err == nil || !strings.Contains(err.Error(), "could not listen on 203.0.113.1:1") {
		t.Errorf("err = %v, want the listen error", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("a bind error other than address-in-use waited for a winner")
	}
}
