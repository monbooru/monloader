package desktop

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Instance struct {
	App     string `json:"app"`
	Version string `json:"version"`
}

const maxHealthBody = 4 << 10

// Probe reports found false only when nothing answered, the one case where
// binding is safe.
func Probe(addr string, timeout time.Duration) (inst Instance, found bool) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return Instance{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Instance{}, true
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHealthBody))
	if err != nil {
		return Instance{}, true
	}
	_ = json.Unmarshal(body, &inst)
	return inst, true
}

func LoopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// LocalRequest reports a request from this machine's own browser. A proxy
// header refuses it, since behind a same-host proxy every peer is loopback,
// and so does a non-loopback Host: a page rebound to 127.0.0.1 sends one.
func LocalRequest(r *http.Request) bool {
	if !IsLoopbackAddr(r.Host) {
		return false
	}
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "Via"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return IsLoopbackAddr(r.RemoteAddr)
}

// A page that rebinds its own name to 127.0.0.1 sends that name, and with
// no password it would otherwise drive every route.
func ForeignHost(r *http.Request, bindAddr, baseURL string) bool {
	return IsLoopbackAddr(bindAddr) && !IsLoopbackAddr(r.Host) && !sameHost(r.Host, baseURL)
}

func sameHost(host, baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return strings.EqualFold(strings.Trim(h, "[]"), u.Hostname())
}

func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
