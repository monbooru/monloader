package desktop

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"
)

// Instance is what answered on /health. App is empty when something
// replied that is not one of ours, which is the case a second launch has
// to report rather than retry.
type Instance struct {
	App     string `json:"app"`
	Version string `json:"version"`
}

// maxHealthBody bounds what Probe reads off a stranger on the port.
const maxHealthBody = 4 << 10

// Probe asks addr's /health who is listening. found is false only when
// nothing answered, which is the one case where binding is safe.
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

// LoopbackAddr rewrites a wildcard bind address to the loopback interface,
// so a probe or a browser open reaches the instance rather than a host
// that does not resolve.
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

// IsLoopbackAddr reports whether a bind address serves the local machine
// only. The setup wizard reads it to preselect the reach choice for the
// bind the config carries.
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
