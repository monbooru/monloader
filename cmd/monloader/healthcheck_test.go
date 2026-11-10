package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHealthAddr(t *testing.T) {
	const envKey = "MONLOADER_SERVER_BIND_ADDRESS"
	dir := t.TempDir()
	cfg := filepath.Join(dir, "monloader.toml")
	if err := os.WriteFile(cfg, []byte("[server]\nbind_address = \"127.0.0.1:9999\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct{ name, env, config, want string }{
		{"env wins", "127.0.0.1:8081", cfg, "127.0.0.1:8081"},
		{"wildcard to loopback", "0.0.0.0:8081", "", "127.0.0.1:8081"},
		{"ipv6 wildcard", "[::]:8081", "", "127.0.0.1:8081"},
		{"config fallback", "", cfg, "127.0.0.1:9999"},
		{"default", "", "", "127.0.0.1:8456"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envKey, tc.env)
			if got := resolveHealthAddr(tc.config); got != tc.want {
				t.Errorf("resolveHealthAddr(%q) env=%q = %q, want %q", tc.config, tc.env, got, tc.want)
			}
		})
	}
}
