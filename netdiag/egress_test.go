package netdiag

import (
	"net/netip"
	"testing"
)

func obs(m EgressMethod, ip string) EgressObservation {
	o := EgressObservation{Method: m}
	if ip != "" {
		o.IP = netip.MustParseAddr(ip)
	}
	return o
}

// TestEgressDivergenceSeverity pins the distinction the verdict depends on:
// STUN disagreeing with itself is a hard failure for hole punching, whereas
// HTTP-only disagreement is a proxy artefact and must stay a warning.
func TestEgressDivergenceSeverity(t *testing.T) {
	cases := []struct {
		name          string
		obs           []EgressObservation
		wantDivergent bool
		wantSTUN      bool
		wantStatus    Status
	}{
		{
			name:       "single egress",
			obs:        []EgressObservation{obs(MethodSTUN, "1.2.3.4"), obs(MethodHTTPv4, "1.2.3.4")},
			wantStatus: StatusOK,
		},
		{
			name: "dual stack is not divergence",
			obs: []EgressObservation{
				obs(MethodSTUN, "1.2.3.4"), obs(MethodHTTPv6, "2001:db8::1"),
			},
			wantStatus: StatusOK,
		},
		{
			name: "http-only split warns",
			obs: []EgressObservation{
				obs(MethodSTUN, "1.2.3.4"),
				obs(MethodHTTPv4, "5.6.7.8"),
			},
			wantDivergent: true,
			wantSTUN:      false,
			wantStatus:    StatusWarn,
		},
		{
			name: "stun split fails",
			obs: []EgressObservation{
				obs(MethodSTUN, "1.2.3.4"),
				obs(MethodSTUN, "5.6.7.8"),
			},
			wantDivergent: true,
			wantSTUN:      true,
			wantStatus:    StatusFail,
		},
		{
			name: "proxy split alone stays a warning",
			obs: []EgressObservation{
				obs(MethodSTUN, "1.2.3.4"),
				obs(MethodHTTPProxy, "9.9.9.9"),
			},
			wantDivergent: true,
			wantSTUN:      false,
			wantStatus:    StatusWarn,
		},
		{
			name:       "no observations fails",
			obs:        []EgressObservation{obs(MethodSTUN, "")},
			wantStatus: StatusFail,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := EgressReport{Observations: tc.obs}
			rep.UniqueIPs = egUniqueIPs(rep.Observations)
			rep.Divergent = egDivergent(rep.UniqueIPs)
			rep.DivergentSTUN = egDivergentSTUN(rep.Observations)
			egFinish(&rep)

			if rep.Divergent != tc.wantDivergent {
				t.Errorf("Divergent = %v, want %v", rep.Divergent, tc.wantDivergent)
			}
			if rep.DivergentSTUN != tc.wantSTUN {
				t.Errorf("DivergentSTUN = %v, want %v", rep.DivergentSTUN, tc.wantSTUN)
			}
			if rep.Status != tc.wantStatus {
				t.Errorf("Status = %v, want %v (summary: %s)", rep.Status, tc.wantStatus, rep.Summary)
			}
		})
	}
}

// A STUN observation that errored carries no address and must not be mistaken
// for a second egress.
func TestEgressDivergentSTUNIgnoresErrors(t *testing.T) {
	o := []EgressObservation{
		obs(MethodSTUN, "1.2.3.4"),
		{Method: MethodSTUN, Err: "timeout"},
	}
	if egDivergentSTUN(o) {
		t.Error("a failed STUN probe must not count as a second egress IP")
	}
}

// egFinish runs again after geolocation, so it must not drift.
func TestEgFinishIdempotent(t *testing.T) {
	rep := EgressReport{Observations: []EgressObservation{
		obs(MethodSTUN, "1.2.3.4"), obs(MethodSTUN, "5.6.7.8"),
	}}
	rep.UniqueIPs = egUniqueIPs(rep.Observations)
	rep.Divergent = egDivergent(rep.UniqueIPs)
	rep.DivergentSTUN = egDivergentSTUN(rep.Observations)

	egFinish(&rep)
	first, status := rep.Summary, rep.Status
	egFinish(&rep)

	if rep.Summary != first || rep.Status != status {
		t.Errorf("egFinish is not idempotent:\n first: %s (%v)\nsecond: %s (%v)",
			first, status, rep.Summary, rep.Status)
	}
}

// TestHeadlineDivergence checks the two verdict strings the user sees.
//
// The report is otherwise healthy: earlier branches (blocked UDP, symmetric
// NAT, unreachable overseas) all outrank egress and would mask it.
func healthyReport(eg EgressReport) *Report {
	return &Report{
		UDP:      UDPReport{V4OK: true},
		NAT:      NATReport{Type: NATFullCone},
		Overseas: OverseasReport{Status: StatusOK},
		Egress:   eg,
	}
}

func TestHeadlineDivergence(t *testing.T) {
	strong := healthyReport(EgressReport{Divergent: true, DivergentSTUN: true})
	if got, lvl := headline(strong); got != "STUN 检测到多个出口 IP，代理或分流工具正在影响连接" {
		t.Errorf("strong headline = %q", got)
	} else if lvl != StatusFail {
		t.Errorf("strong headline severity = %v, want fail", lvl)
	}
	weak := healthyReport(EgressReport{Divergent: true})
	if got, lvl := headline(weak); got != "仅 HTTP 探测到多个出口 IP，代理或分流工具可能影响连接" {
		t.Errorf("weak headline = %q", got)
	} else if lvl != StatusWarn {
		t.Errorf("weak headline severity = %v, want warn", lvl)
	}
}
