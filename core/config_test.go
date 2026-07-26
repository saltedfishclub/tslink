package core

import (
	"strings"
	"testing"
)

func TestConnectRuleBindIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rule ConnectRule
		want string
	}{
		{
			name: "default local only",
			rule: ConnectRule{Protocol: "tcp"},
			want: "127.0.0.1",
		},
		{
			name: "explicit local addr",
			rule: ConnectRule{Protocol: "udp", LocalAddr: "192.168.1.10"},
			want: "192.168.1.10",
		},
		{
			name: "minecraft exposes LAN by default",
			rule: ConnectRule{Protocol: "minecraft"},
			want: "0.0.0.0",
		},
		{
			name: "lan enable exposes LAN",
			rule: ConnectRule{Protocol: "tcp", LanEnable: boolPtr(true)},
			want: "0.0.0.0",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.rule.BindIP(); got != tt.want {
				t.Fatalf("BindIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigValidateAcceptsValidConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Core: Core{AuthKey: "tskey-auth-example"},
		DNS:  DNS{DoHServers: []string{"https://cloudflare-dns.com/dns-query"}},
		Forward: map[string][]ForwardRule{
			"web": {
				{Protocol: "tcp", TailscalePort: 8080, LocalAddr: "127.0.0.1:9090"},
				{Protocol: "udp", TailscalePort: 8080, LocalAddr: "127.0.0.1:9090"},
			},
		},
		Connect: map[string][]ConnectRule{
			"api": {
				{Protocol: "tcp", LocalPort: 9000, DstAddr: "host.ts.net:8080"},
				{Protocol: "udp", LocalPort: 9000, DstAddr: "host.ts.net:8080"},
			},
		},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() returned error: %v", err)
	}
}

func TestConfigValidateRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Core: Core{},
		Forward: map[string][]ForwardRule{
			"bad": {
				{Protocol: "icmp", TailscalePort: 70000, LocalAddr: "127.0.0.1"},
			},
		},
		Connect: map[string][]ConnectRule{
			"bad": {
				{Protocol: "tcp", LocalPort: 9000, DstAddr: "host.ts.net:8080"},
				{Protocol: "minecraft", LocalPort: 9000, DstAddr: "host.ts.net:25565"},
			},
		},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() returned nil, want error")
	}

	for _, want := range []string{
		"core.auth_key is required",
		"forward.bad[0].protocol must be tcp or udp",
		"forward.bad[0].tailscale_port must be between 1 and 65535",
		"forward.bad[0].local_addr invalid",
		"connect.bad[1] local listener duplicates connect.bad[0]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate() error %q does not contain %q", err.Error(), want)
		}
	}
}

func TestConfigValidateRejectsInvalidDoHServer(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Core: Core{AuthKey: "tskey-auth-example"},
		DNS:  DNS{DoHServers: []string{"https://ok.example/dns-query", "not a url", "ftp://wrong.example"}},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() returned nil, want error")
	}

	for _, want := range []string{
		"dns.doh_servers[1] must be a valid http(s) URL",
		"dns.doh_servers[2] must be a valid http(s) URL",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate() error %q does not contain %q", err.Error(), want)
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}
