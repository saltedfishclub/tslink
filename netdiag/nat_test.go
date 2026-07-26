package netdiag

import (
	"net/netip"
	"testing"
)

func ap(s string) netip.AddrPort {
	return netip.MustParseAddrPort(s)
}

// TestSTUNChangeHonoured pins the rule that decides whether a CHANGE-REQUEST
// reply is evidence of anything. The middlebox case is the one that matters:
// answering CHANGE-IP|CHANGE-PORT from the *same IP* on another port used to
// count as success, which reported a port-restricted line as a full cone.
func TestSTUNChangeHonoured(t *testing.T) {
	const dst = "203.0.113.10:3478"
	cases := []struct {
		name   string
		change byte
		from   string
		want   bool
	}{
		{"change ip+port, alternate ip and port", stunChangeIP | stunChangePort, "203.0.113.11:3479", true},
		{"change ip+port, ignored", stunChangeIP | stunChangePort, "203.0.113.10:3478", false},
		{"change ip+port, only port moved", stunChangeIP | stunChangePort, "203.0.113.10:443", false},
		{"change ip+port, only ip moved", stunChangeIP | stunChangePort, "203.0.113.11:3478", false},
		{"change port, another port same ip", stunChangePort, "203.0.113.10:3479", true},
		{"change port, ignored", stunChangePort, "203.0.113.10:3478", false},
		{"change port, ip moved too", stunChangePort, "203.0.113.11:3479", false},
		{"change ip, another ip same port", stunChangeIP, "203.0.113.11:3478", true},
		{"change ip, port moved too", stunChangeIP, "203.0.113.11:3479", false},
		{"no flags is never honoured", 0, "203.0.113.11:3479", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stunChangeHonoured(tc.change, ap(dst), ap(tc.from)); got != tc.want {
				t.Fatalf("stunChangeHonoured(%#x, %s, %s) = %v, want %v",
					tc.change, dst, tc.from, got, tc.want)
			}
		})
	}
	if stunChangeHonoured(stunChangeIP, ap(dst), netip.AddrPort{}) {
		t.Fatal("an invalid source address must never count as honoured")
	}
}

func TestSTUNWithRFC5780(t *testing.T) {
	// A caller list of endpoints that ignore CHANGE-REQUEST must still end up
	// with candidates that can drive the filtering test.
	custom := []STUNServer{{Host: "stun.l.google.com:19302", Name: "Google", Region: RegionIntl}}
	got := stunWithRFC5780(custom)
	if len(got) != 1+len(RFC5780Servers()) {
		t.Fatalf("got %d servers, want %d", len(got), 1+len(RFC5780Servers()))
	}
	if got[0].Host != custom[0].Host {
		t.Errorf("caller's servers must stay first, got %s", got[0].Host)
	}
	if len(custom) != 1 {
		t.Errorf("caller's slice was mutated: %v", custom)
	}

	// The default list already covers them, so nothing should be appended and
	// no host may repeat.
	def := DefaultSTUNServers()
	merged := stunWithRFC5780(def)
	if len(merged) != len(def) {
		t.Errorf("default list gained %d servers, want 0", len(merged)-len(def))
	}
	seen := map[string]bool{}
	for _, s := range merged {
		if seen[s.Host] {
			t.Errorf("duplicate host %s", s.Host)
		}
		seen[s.Host] = true
	}
}

// TestFilteringCandidates checks the ordering that makes the filtering test
// affordable: servers already known to advertise an OTHER-ADDRESS first,
// unprobed servers next, and servers proven to advertise nothing dropped so
// they cannot burn two CHANGE-REQUEST timeouts each.
func TestFilteringCandidates(t *testing.T) {
	mk := func(host, addr string) stunTarget {
		return stunTarget{srv: STUNServer{Host: host, Name: host}, dst: ap(addr)}
	}
	capable := mk("capable", "203.0.113.1:3478")
	barren := mk("barren", "203.0.113.2:3478")
	unprobed := mk("unprobed", "203.0.113.3:3478")

	c := &natClassifier{other: map[netip.AddrPort]netip.AddrPort{
		capable.dst: ap("203.0.113.9:3479"),
		barren.dst:  {}, // answered, advertised no alternate address
	}}

	got := c.filteringCandidates([]stunTarget{barren, unprobed, capable})
	var hosts []string
	for _, t := range got {
		hosts = append(hosts, t.srv.Host)
	}
	want := []string{"capable", "unprobed"}
	if len(hosts) != len(want) {
		t.Fatalf("candidates = %v, want %v", hosts, want)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", hosts, want)
		}
	}
}

// TestSTUNLegacyNATType covers the RFC 5780 -> RFC 3489 mapping, including the
// rule that unknown filtering is never guessed away.
func TestSTUNLegacyNATType(t *testing.T) {
	cases := []struct {
		noNAT     bool
		mapping   Behavior
		filtering Behavior
		want      NATType
	}{
		{false, BehaviorEndpointIndependent, BehaviorEndpointIndependent, NATFullCone},
		{false, BehaviorEndpointIndependent, BehaviorAddressDependent, NATRestricted},
		{false, BehaviorEndpointIndependent, BehaviorAddressAndPortDependent, NATPortRestrict},
		{false, BehaviorEndpointIndependent, BehaviorUnknown, NATUnknown},
		{false, BehaviorAddressDependent, BehaviorEndpointIndependent, NATSymmetric},
		{false, BehaviorAddressAndPortDependent, BehaviorUnknown, NATSymmetric},
		{false, BehaviorUnknown, BehaviorEndpointIndependent, NATUnknown},
		{true, BehaviorEndpointIndependent, BehaviorEndpointIndependent, NATOpen},
		{true, BehaviorEndpointIndependent, BehaviorAddressDependent, NATSymmetricFW},
		{true, BehaviorEndpointIndependent, BehaviorUnknown, NATUnknown},
	}
	for _, tc := range cases {
		got := stunLegacyNATType(tc.noNAT, tc.mapping, tc.filtering)
		if got != tc.want {
			t.Errorf("stunLegacyNATType(%v, %s, %s) = %s, want %s",
				tc.noNAT, tc.mapping, tc.filtering, got, tc.want)
		}
	}
}
