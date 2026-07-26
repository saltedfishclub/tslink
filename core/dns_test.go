package core

import (
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

// peerStatus builds a PeerStatus with the given tailnet address and advertised
// routes. Passing no routes leaves AllowedIPs nil, as it is for peers that
// advertise nothing.
func peerStatus(tailIP string, routes ...string) *ipnstate.PeerStatus {
	ps := &ipnstate.PeerStatus{
		TailscaleIPs: []netip.Addr{netip.MustParseAddr(tailIP)},
	}
	if len(routes) > 0 {
		prefixes := make([]netip.Prefix, 0, len(routes))
		for _, r := range routes {
			prefixes = append(prefixes, netip.MustParsePrefix(r))
		}
		s := views.SliceOf(prefixes)
		ps.AllowedIPs = &s
	}
	return ps
}

func statusWithPeers(peers ...*ipnstate.PeerStatus) *ipnstate.Status {
	st := &ipnstate.Status{Peer: make(map[key.NodePublic]*ipnstate.PeerStatus, len(peers))}
	for _, p := range peers {
		st.Peer[key.NewNode().Public()] = p
	}
	return st
}

func TestPeerCarryingIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		peers []*ipnstate.PeerStatus
		ip    string
		want  string // "" means no peer expected
	}{
		{
			name:  "peer's own address",
			peers: []*ipnstate.PeerStatus{peerStatus("100.64.0.1", "100.64.0.1/32")},
			ip:    "100.64.0.1",
			want:  "100.64.0.1",
		},
		{
			name: "subnet router carries a LAN address",
			peers: []*ipnstate.PeerStatus{
				peerStatus("100.64.0.2", "100.64.0.2/32", "10.0.0.0/24"),
				peerStatus("100.64.0.3", "100.64.0.3/32"),
			},
			ip:   "10.0.0.7",
			want: "100.64.0.2",
		},
		{
			// An exit node advertises 0.0.0.0/0, which Contains every address.
			// Matching it would pick a peer at random out of map iteration order.
			name: "exit node does not shadow the real subnet router",
			peers: []*ipnstate.PeerStatus{
				peerStatus("100.64.0.9", "0.0.0.0/0", "::/0"),
				peerStatus("100.64.0.2", "10.0.0.0/24"),
			},
			ip:   "10.0.0.7",
			want: "100.64.0.2",
		},
		{
			name: "most specific route wins",
			peers: []*ipnstate.PeerStatus{
				peerStatus("100.64.0.4", "10.0.0.0/8"),
				peerStatus("100.64.0.5", "10.0.0.0/24"),
			},
			ip:   "10.0.0.7",
			want: "100.64.0.5",
		},
		{
			name: "equal routes break the tie deterministically",
			peers: []*ipnstate.PeerStatus{
				peerStatus("100.64.0.8", "10.0.0.0/24"),
				peerStatus("100.64.0.6", "10.0.0.0/24"),
			},
			ip:   "10.0.0.7",
			want: "100.64.0.6",
		},
		{
			name:  "public address belongs to no peer",
			peers: []*ipnstate.PeerStatus{peerStatus("100.64.0.1", "10.0.0.0/24")},
			ip:    "1.1.1.1",
			want:  "",
		},
		{
			name:  "peer without AllowedIPs is skipped, not dereferenced",
			peers: []*ipnstate.PeerStatus{peerStatus("100.64.0.1")},
			ip:    "10.0.0.7",
			want:  "",
		},
		{
			name:  "exit node alone still does not match",
			peers: []*ipnstate.PeerStatus{peerStatus("100.64.0.9", "0.0.0.0/0")},
			ip:    "1.1.1.1",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st := statusWithPeers(tt.peers...)
			// Run repeatedly: map iteration order is unspecified, so a result
			// that depends on it shows up as a flake here.
			for range 20 {
				got, ok := peerCarryingIP(st, netip.MustParseAddr(tt.ip))
				if tt.want == "" {
					if ok {
						t.Fatalf("peerCarryingIP() = %v, true; want no match", got)
					}
					continue
				}
				if !ok {
					t.Fatalf("peerCarryingIP() = _, false; want %s", tt.want)
				}
				if got.String() != tt.want {
					t.Fatalf("peerCarryingIP() = %s, want %s", got, tt.want)
				}
			}
		})
	}
}
