package core

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
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

func (r ConnectRule) BindIP() string {
	if r.LANEnabled() {
		return "0.0.0.0"
	}
	if r.LocalAddr != "" {
		return r.LocalAddr
	}
	return "127.0.0.1"
}

type Config struct {
	Core    Core                     `toml:"core"`
	Forward map[string][]ForwardRule `toml:"forward"`
	Connect map[string][]ConnectRule `toml:"connect"`
}

func (cfg *Config) ApplyDefaults() {
	if cfg.Forward == nil {
		cfg.Forward = make(map[string][]ForwardRule)
	}
	if cfg.Connect == nil {
		cfg.Connect = make(map[string][]ConnectRule)
	}
	if cfg.Core.Hostname == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown"
		}
		cfg.Core.Hostname = hostname
	}
}

func (cfg *Config) Validate() error {
	var errs []error

	if strings.TrimSpace(cfg.Core.AuthKey) == "" {
		errs = append(errs, errors.New("core.auth_key is required"))
	}

	usedForwardListeners := make(map[string]string)
	usedConnectListeners := make(map[string]string)

	for tag, rules := range cfg.Forward {
		for i, rule := range rules {
			path := fmt.Sprintf("forward.%s[%d]", tag, i)
			if rule.Protocol != "tcp" && rule.Protocol != "udp" {
				errs = append(errs, fmt.Errorf("%s.protocol must be tcp or udp", path))
			}
			if !validPort(rule.TailscalePort) {
				errs = append(errs, fmt.Errorf("%s.tailscale_port must be between 1 and 65535", path))
			} else {
				key := fmt.Sprintf("%s:%d", rule.Protocol, rule.TailscalePort)
				if prev, ok := usedForwardListeners[key]; ok {
					errs = append(errs, fmt.Errorf("%s.tailscale_port duplicates %s", path, prev))
				} else {
					usedForwardListeners[key] = path
				}
			}
			if strings.TrimSpace(rule.LocalAddr) == "" {
				errs = append(errs, fmt.Errorf("%s.local_addr is required", path))
			} else if err := validateHostPort(rule.LocalAddr); err != nil {
				errs = append(errs, fmt.Errorf("%s.local_addr invalid: %w", path, err))
			}
		}
	}

	for tag, rules := range cfg.Connect {
		for i, rule := range rules {
			path := fmt.Sprintf("connect.%s[%d]", tag, i)
			if rule.Protocol != "tcp" && rule.Protocol != "udp" && rule.Protocol != "minecraft" {
				errs = append(errs, fmt.Errorf("%s.protocol must be tcp, udp, or minecraft", path))
			}
			if !validPort(rule.LocalPort) {
				errs = append(errs, fmt.Errorf("%s.local_port must be between 1 and 65535", path))
			}
			if strings.TrimSpace(rule.DstAddr) == "" {
				errs = append(errs, fmt.Errorf("%s.dst_addr is required", path))
			} else if err := validateHostPort(rule.DstAddr); err != nil {
				errs = append(errs, fmt.Errorf("%s.dst_addr invalid: %w", path, err))
			}
			if rule.LocalAddr != "" && net.ParseIP(rule.LocalAddr) == nil {
				errs = append(errs, fmt.Errorf("%s.local_addr must be an IP address", path))
			}
			if validPort(rule.LocalPort) && (rule.Protocol == "tcp" || rule.Protocol == "udp" || rule.Protocol == "minecraft") {
				network := rule.Protocol
				if network == "minecraft" {
					network = "tcp"
				}
				if prev, ok := conflictingListener(usedConnectListeners, network, rule.BindIP(), rule.LocalPort); ok {
					errs = append(errs, fmt.Errorf("%s local listener duplicates %s", path, prev))
				}
				usedConnectListeners[listenerKey(network, rule.BindIP(), rule.LocalPort)] = path
			}
		}
	}

	return errors.Join(errs...)
}

func validPort(port int) bool {
	return port > 0 && port <= 65535
}

func validateHostPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if strings.TrimSpace(host) == "" {
		return errors.New("host is required")
	}
	if strings.TrimSpace(port) == "" {
		return errors.New("port is required")
	}
	return nil
}

func listenerKey(network, ip string, port int) string {
	return fmt.Sprintf("%s/%s", network, net.JoinHostPort(ip, fmt.Sprintf("%d", port)))
}

func conflictingListener(used map[string]string, network, ip string, port int) (string, bool) {
	candidates := []string{
		listenerKey(network, ip, port),
	}
	if ip == "0.0.0.0" {
		for key, path := range used {
			prefix := network + "/"
			_, usedPort, err := net.SplitHostPort(strings.TrimPrefix(key, prefix))
			if strings.HasPrefix(key, prefix) && err == nil && usedPort == fmt.Sprintf("%d", port) {
				return path, true
			}
		}
	} else {
		candidates = append(candidates, listenerKey(network, "0.0.0.0", port))
	}
	for _, key := range candidates {
		if prev, ok := used[key]; ok {
			return prev, true
		}
	}
	return "", false
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

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
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

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}
