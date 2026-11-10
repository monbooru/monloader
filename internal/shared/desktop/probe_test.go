package desktop

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func probeStub(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestProbeIdentifiesOurOwnInstance(t *testing.T) {
	addr := probeStub(t, http.StatusOK, `{"app":"monbooru","status":"ok","version":"v1.19.1"}`)
	inst, found := Probe(addr, time.Second)
	if !found {
		t.Fatal("found = false against a live listener")
	}
	if inst.App != "monbooru" || inst.Version != "v1.19.1" {
		t.Errorf("got %+v", inst)
	}
}

func TestProbeReportsAStrangerOnThePort(t *testing.T) {
	addr := probeStub(t, http.StatusOK, `<html>not us</html>`)
	inst, found := Probe(addr, time.Second)
	if !found {
		t.Fatal("found = false against a live listener")
	}
	if inst.App != "" {
		t.Errorf("App = %q, want empty for a stranger", inst.App)
	}
}

func TestProbeTreatsANonSuccessAsOccupied(t *testing.T) {
	addr := probeStub(t, http.StatusUnauthorized, "")
	if _, found := Probe(addr, time.Second); !found {
		t.Error("found = false on a 401; the port is occupied either way")
	}
}

func TestProbeReportsAFreePort(t *testing.T) {
	if _, found := Probe(freeAddr(t), 500*time.Millisecond); found {
		t.Error("found = true with nothing listening")
	}
}

func TestLocalRequest(t *testing.T) {
	local := func() *http.Request {
		r := httptest.NewRequest("GET", "/x", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		r.Host = "127.0.0.1:8455"
		return r
	}
	if !LocalRequest(local()) {
		t.Fatal("a loopback request was refused")
	}
	for _, header := range []string{"X-Forwarded-For", "Forwarded", "X-Real-IP", "Via"} {
		r := local()
		r.Header.Set(header, "192.168.1.20")
		if LocalRequest(r) {
			t.Errorf("%s: a proxied request passed as the local machine", header)
		}
	}
	for header, value := range map[string]string{"Origin": "http://booru.example", "Referer": "https://booru.example/settings"} {
		r := local()
		r.Header.Set(header, value)
		if LocalRequest(r) {
			t.Errorf("%s naming another host passed as the local machine", header)
		}
	}
	for _, value := range []string{"http://127.0.0.1:8455", "http://localhost:8455/settings"} {
		r := local()
		r.Header.Set("Origin", value)
		if !LocalRequest(r) {
			t.Errorf("Origin %s was refused", value)
		}
	}
	r := local()
	r.Host = "evil.example:8455"
	if LocalRequest(r) {
		t.Error("a rebound Host passed")
	}
	r = local()
	r.Host = "localhost:8455"
	if !LocalRequest(r) {
		t.Error("localhost was refused")
	}
	r = local()
	r.RemoteAddr = "192.168.1.20:54321"
	if LocalRequest(r) {
		t.Error("a LAN peer passed")
	}
}

func TestProbeGoesDirect(t *testing.T) {
	if probeTransport.Proxy != nil {
		t.Error("an instance probe follows a proxy, which answers for a LAN address")
	}
}

func TestForeignHost(t *testing.T) {
	for _, tc := range []struct {
		bind, base, host, path string
		want                   bool
	}{
		{"127.0.0.1:8455", "http://localhost:8455", "evil.example:8455", "/", true},
		{"127.0.0.1:8455", "http://localhost:8455", "evil.example:8455", "/api/v1/queue", true},
		{"127.0.0.1:8455", "http://localhost:8455", "127.0.0.1:8455", "/", false},
		{"127.0.0.1:8455", "http://localhost:8455", "localhost:8455", "/", false},
		{"127.0.0.1:8455", "http://localhost:8455", "[::1]:8455", "/", false},
		{"127.0.0.1:8455", "http://box.lan:8455", "BOX.lan:8455", "/", false},
		{"127.0.0.1:8455", "http://box.lan:8455", "box.lan", "/", false},
		{"127.0.0.1:8455", "", "box.lan:8455", "/", true},
		{"0.0.0.0:8455", "http://localhost:8455", "nas.lan:8455", "/", false},
		{"192.168.1.5:8455", "", "nas.lan:8455", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "evil.example:8455", "/", true},
		{"0.0.0.0:8455", "http://localhost:8455", "evil.example", "/settings/raw", true},
		{"0.0.0.0:8455", "http://localhost:8455", "EVIL.example.:8455", "/", true},
		{"[::]:8455", "http://localhost:8455", "evil.example:8455", "/", true},
		{"0.0.0.0:8455", "http://localhost:8455", "evil.example:8455", "/api/v1/queue", false},
		{"0.0.0.0:8455", "http://localhost:8455", "evil.example:8455", "/health", false},
		{"0.0.0.0:8455", "https://photos.example.com", "photos.example.com", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "nas", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "nas.local:8455", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "NAS.LAN.:8455", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "box.home.arpa", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "box.tailnet.ts.net:8455", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "app.localhost:8455", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "192.168.1.5:8455", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "100.64.0.1", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "[fd00::1]:8455", "/", false},
		{"0.0.0.0:8455", "http://localhost:8455", "notlan.example:8455", "/", true},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Host = tc.host
		if got := ForeignHost(r, tc.bind, tc.base); got != tc.want {
			t.Errorf("bind %s base %q Host %s path %s: ForeignHost = %v, want %v", tc.bind, tc.base, tc.host, tc.path, got, tc.want)
		}
	}
}
