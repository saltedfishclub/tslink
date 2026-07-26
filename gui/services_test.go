package gui

import (
	"testing"

	"tslink/core"
)

// TestBuildServicesGroupsByHost pins the two properties the old LAN page got
// wrong: one entry per server (not per rule, and not per IP family), and an
// order that does not depend on map iteration.
func TestBuildServicesGroupsByHost(t *testing.T) {
	cfg := &core.Config{
		Connect: map[string][]core.ConnectRule{
			// Same destination host over two protocols: must collapse into one
			// server carrying two services.
			"l4d2_tcp": {{Protocol: "tcp", LocalPort: 27015, DstAddr: "server.l4d2.example:27015"}},
			"l4d2_udp": {{Protocol: "udp", LocalPort: 27015, DstAddr: "server.l4d2.example:27015"}},
			"sfcraft":  {{Protocol: "minecraft", LocalPort: 25566, DstAddr: "a.mc.example:25565", LanMotd: "SFCraft"}},
			// Not a tailnet host; it still has to render.
			"voice": {{Protocol: "udp", LocalPort: 24454, DstAddr: "mc.lxns.net:24454"}},
		},
	}

	got := buildServices(cfg, core.PeerSnapshot{})

	if len(got) != 3 {
		t.Fatalf("want 3 servers, got %d: %+v", len(got), got)
	}

	// Sorted by host.
	wantHosts := []string{"a.mc.example", "mc.lxns.net", "server.l4d2.example"}
	for i, want := range wantHosts {
		if got[i].Host != want {
			t.Errorf("server[%d].Host = %q, want %q", i, got[i].Host, want)
		}
	}

	l4d2 := got[2]
	if len(l4d2.Services) != 2 {
		t.Fatalf("l4d2 should carry both protocols, got %d", len(l4d2.Services))
	}
	if l4d2.Services[0].Proto != "tcp" || l4d2.Services[1].Proto != "udp" {
		t.Errorf("services not ordered by protocol: %+v", l4d2.Services)
	}
	// No peer resolved: must not claim the server is down.
	if !l4d2.Online() {
		t.Error("a server with no resolved peer should not render as offline")
	}
	if l4d2.Title() != "server.l4d2.example" {
		t.Errorf("Title() = %q, want the host", l4d2.Title())
	}

	// LANEnabled defaults to true only for minecraft.
	mc := got[0]
	if !mc.Services[0].Broadcast {
		t.Error("a minecraft rule should be marked as broadcast")
	}
	if mc.Services[0].Name != "SFCraft" {
		t.Errorf("Name = %q, want the lan_motd", mc.Services[0].Name)
	}
	if got[1].Services[0].Broadcast {
		t.Error("a plain udp rule should not be marked as broadcast")
	}

	// Repeated builds must agree, or the section jitters between frames.
	for i := 0; i < 20; i++ {
		again := buildServices(cfg, core.PeerSnapshot{})
		for j := range again {
			if again[j].Host != got[j].Host {
				t.Fatalf("ordering is unstable: %q vs %q", again[j].Host, got[j].Host)
			}
		}
	}
}

// TestBuildServicesUsesPeer checks the enrichment path: a resolved peer supplies
// the display name and the online state.
func TestBuildServicesUsesPeer(t *testing.T) {
	cfg := &core.Config{
		Connect: map[string][]core.ConnectRule{
			"sfcraft": {{Protocol: "minecraft", LocalPort: 25566, DstAddr: "a.mc.example:25565"}},
		},
	}
	snap := core.PeerSnapshot{Peers: []core.PeerInfo{{
		ID: "n1", DisplayName: "homelab", Online: false,
		Linked: true, LinkTags: []string{"sfcraft"},
	}}}

	got := buildServices(cfg, snap)
	if len(got) != 1 {
		t.Fatalf("want 1 server, got %d", len(got))
	}
	if got[0].Title() != "homelab" {
		t.Errorf("Title() = %q, want the peer display name", got[0].Title())
	}
	if got[0].Online() {
		t.Error("an offline peer should make the server render as offline")
	}
}

func TestBuildServicesNilConfig(t *testing.T) {
	if got := buildServices(nil, core.PeerSnapshot{}); got != nil {
		t.Errorf("want nil for a nil config, got %+v", got)
	}
}
