package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/monbooru/monloader/internal/desktop"
)

func runHealthcheck(argv []string) {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	configPath := fs.String("config", "", "optional monloader.toml to read server.bind_address from")
	timeout := fs.Duration("timeout", 3*time.Second, "probe timeout")
	_ = fs.Parse(argv)

	url := "http://" + resolveHealthAddr(*configPath) + "/health"
	resp, err := (&http.Client{Timeout: *timeout}).Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck: %s -> %d\n", url, resp.StatusCode)
		os.Exit(1)
	}
}

// Not config.LoadWithDefaults, which writes a default config where none exists.
func resolveHealthAddr(configPath string) string {
	addr := os.Getenv("MONLOADER_SERVER_BIND_ADDRESS")
	if addr == "" && configPath != "" {
		var mc struct {
			Server struct {
				BindAddress string `toml:"bind_address"`
			} `toml:"server"`
		}
		if _, err := toml.DecodeFile(configPath, &mc); err == nil {
			addr = mc.Server.BindAddress
		}
	}
	if addr == "" {
		addr = "127.0.0.1:8456"
	}
	return desktop.LoopbackAddr(addr)
}
