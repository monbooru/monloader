package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDrainHTTPWaitsForInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			done <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		done <- result{body: string(b)}
	}()
	<-started

	drained := make(chan error, 1)
	go func() { drained <- drainHTTP(srv, 5*time.Second) }()

	select {
	case err := <-drained:
		t.Fatalf("drain returned while a request was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	if got := <-done; got.err != nil || got.body != "finished" {
		t.Errorf("in-flight request = %q, %v; want it to complete", got.body, got.err)
	}
	if err := <-drained; err != nil {
		t.Errorf("drain returned %v", err)
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/after"); err == nil {
		t.Error("the server kept accepting after the drain")
	}
}

func TestDrainHTTPGivesUpPastTheTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/stuck"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started

	if err := drainHTTP(srv, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("drain error = %v, want the deadline to expire", err)
	}
}
