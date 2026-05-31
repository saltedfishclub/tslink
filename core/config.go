package core

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultConfigURL is the default URL for fetching config.
// Set via build flags: go build -ldflags "-X tslink/core.DefaultConfigURL=https://..."
var DefaultConfigURL string

type ForwardRule struct {
	Protocol      string `toml:"protocol"`
	TailscalePort int    `toml:"tailscale_port"`
	LocalAddr     string `toml:"local_addr"`
}

type ConnectRule struct {
	Protocol  string `toml:"protocol"`
	LocalPort int    `toml:"local_port"`
	LocalAddr string `toml:"local_addr"`
	DstAddr   string `toml:"dst_addr"`
	LanEnable *bool  `toml:"lan_enable"`
	LanMotd   string `toml:"lan_motd"`
}

type Core struct {
	AuthKey      string `toml:"auth_key"`
	ControlURL   string `toml:"control_url"`
	Hostname     string `toml:"hostname"`
	Ephemeral    bool   `toml:"ephemeral"`
	AcceptRoutes bool   `toml:"accept_routes"`
}

func (r ConnectRule) LANEnabled() bool {
	if r.LanEnable != nil {
		return *r.LanEnable
	}
	return r.Protocol == "minecraft"
}

func (r ConnectRule) LANMotdOr(def string) string {
	if r.LanMotd != "" {
		return r.LanMotd
	}
	return def
}

type Config struct {
	Core    Core                     `toml:"core"`
	Forward map[string][]ForwardRule `toml:"forward"`
	Connect map[string][]ConnectRule `toml:"connect"`
}

// LoadConfig loads configuration from a file path or URL.
// If path starts with "http://" or "https://", it fetches the config from the URL.
// Otherwise, it reads from the local file system.
func LoadConfig(path string) (*Config, error) {
	// Detect URL
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return loadConfigFromURL(path)
	}

	// File-based loading
	cfg := &Config{
		Core: Core{
			Hostname:     "",
			Ephemeral:    true,
			AcceptRoutes: true,
		},
		Forward: make(map[string][]ForwardRule),
		Connect: make(map[string][]ConnectRule),
	}
	if _, err := os.Stat(path); err != nil {
		// write a basic config
		buf := new(bytes.Buffer)
		err = toml.NewEncoder(buf).Encode(cfg)
		if err != nil {
			return nil, err
		}
		err = os.WriteFile(path, buf.Bytes(), 0644)
	}
	_, err := toml.DecodeFile(path, cfg)
	if err != nil {
		return nil, err
	}

	if cfg.Core.Hostname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown"
		}
		cfg.Core.Hostname = hostname
	}

	return cfg, nil
}

// loadConfigFromURL fetches a TOML config from the given URL and decodes it.
func loadConfigFromURL(url string) (*Config, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch config from URL %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch config from URL %s: unexpected status %d", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from %s: %w", url, err)
	}

	cfg := &Config{
		Core: Core{
			Hostname:     "",
			Ephemeral:    true,
			AcceptRoutes: true,
		},
		Forward: make(map[string][]ForwardRule),
		Connect: make(map[string][]ConnectRule),
	}

	err = toml.Unmarshal(body, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to decode TOML config from %s: %w", url, err)
	}

	if cfg.Core.Hostname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown"
		}
		cfg.Core.Hostname = hostname
	}

	return cfg, nil
}
