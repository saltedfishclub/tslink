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

// TestGroupServices pins the grouping the overview relies on: hosts fronted by
// one tailnet node collapse into a single group, a public destination stays on
// its own, and the hosts within a group keep buildServices' stable order.
func TestGroupServices(t *testing.T) {
	cfg := &core.Config{
		Connect: map[string][]core.ConnectRule{
			"sfcraft":  {{Protocol: "minecraft", LocalPort: 25566, DstAddr: "sfcraft.mc.homelab.ice:25565"}},
			"mayday":   {{Protocol: "minecraft", LocalPort: 25571, DstAddr: "mayday.mc.homelab.ice:25565"}},
			"l4d2_tcp": {{Protocol: "tcp", LocalPort: 27015, DstAddr: "server.l4d2.homelab.ice:27015"}},
			// A public host resolves to no peer and must stand alone.
			"voice": {{Protocol: "udp", LocalPort: 24454, DstAddr: "mc.lxns.net:24454"}},
		},
	}
	// All three homelab hosts resolve to one subnet router.
	snap := core.PeerSnapshot{Peers: []core.PeerInfo{{
		ID: "n1", DisplayName: "tsdns-homelab", Online: true,
		Linked: true, LinkTags: []string{"sfcraft", "mayday", "l4d2_tcp"},
	}}}

	groups := groupServices(buildServices(cfg, snap))
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d: %+v", len(groups), groups)
	}

	var homelab, relay *serviceGroup
	for i := range groups {
		switch groups[i].Title() {
		case "tsdns-homelab":
			homelab = &groups[i]
		case "mc.lxns.net":
			relay = &groups[i]
		}
	}

	if homelab == nil {
		t.Fatal("no group titled tsdns-homelab")
	}
	if len(homelab.Servers) != 3 {
		t.Fatalf("homelab group should carry 3 hosts, got %d", len(homelab.Servers))
	}
	// Hosts stay host-sorted so the group does not reshuffle between frames.
	wantHosts := []string{"mayday.mc.homelab.ice", "server.l4d2.homelab.ice", "sfcraft.mc.homelab.ice"}
	for i, w := range wantHosts {
		if homelab.Servers[i].Host != w {
			t.Errorf("homelab host[%d] = %q, want %q", i, homelab.Servers[i].Host, w)
		}
	}
	if !homelab.Online() {
		t.Error("a group behind an online peer must not render as offline")
	}

	if relay == nil {
		t.Fatal("no standalone group for the public relay")
	}
	if relay.Peer != nil {
		t.Error("a public destination must not be attached to a peer")
	}
	if len(relay.Servers) != 1 {
		t.Errorf("standalone group should carry 1 host, got %d", len(relay.Servers))
	}

	// Grouping must be deterministic: repeated builds agree, or the section
	// jitters between frames.
	for i := 0; i < 20; i++ {
		again := groupServices(buildServices(cfg, snap))
		if len(again) != len(groups) {
			t.Fatalf("group count is unstable: %d vs %d", len(again), len(groups))
		}
		for j := range again {
			if again[j].Title() != groups[j].Title() {
				t.Fatalf("group order is unstable: %q vs %q", again[j].Title(), groups[j].Title())
			}
		}
	}
}

// TestGroupServicesSeparatesPeers checks that two distinct peers do not merge:
// grouping is by node identity, not by a shared DNS suffix.
func TestGroupServicesSeparatesPeers(t *testing.T) {
	cfg := &core.Config{
		Connect: map[string][]core.ConnectRule{
			"a": {{Protocol: "tcp", LocalPort: 1000, DstAddr: "a.homelab.ice:1000"}},
			"b": {{Protocol: "tcp", LocalPort: 2000, DstAddr: "b.homelab.ice:2000"}},
		},
	}
	snap := core.PeerSnapshot{Peers: []core.PeerInfo{
		{ID: "n1", DisplayName: "box-a", Online: true, Linked: true, LinkTags: []string{"a"}},
		{ID: "n2", DisplayName: "box-b", Online: true, Linked: true, LinkTags: []string{"b"}},
	}}

	groups := groupServices(buildServices(cfg, snap))
	if len(groups) != 2 {
		t.Fatalf("want 2 groups for 2 distinct peers, got %d", len(groups))
	}
	for _, g := range groups {
		if len(g.Servers) != 1 {
			t.Errorf("group %q should carry 1 host, got %d", g.Title(), len(g.Servers))
		}
	}
}
