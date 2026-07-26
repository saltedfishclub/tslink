package netdiag

import (
	"net/netip"
	"strings"
	"testing"
)

// measuredReport is a run where every section measured something and nothing
// measured badly. Individual tests spoil one thing at a time.
func measuredReport() *Report {
	yes, no := true, false
	return &Report{
		Status: StatusOK,
		Interfaces: InterfaceReport{
			Status:       StatusOK,
			DefaultV4Src: netip.MustParseAddr("192.168.1.10"),
		},
		UDP: UDPReport{
			Status: StatusOK, V4OK: true,
			Probes:      []UDPProbe{{Host: "stun.example:3478", OK: true}},
			CNReachable: 2, CNTotal: 2, IntlReachabl: 2, IntlTotal: 2,
		},
		NAT: NATReport{
			Status: StatusOK, Type: NATFullCone,
			Mapping:   BehaviorEndpointIndependent,
			Filtering: BehaviorEndpointIndependent,
			Hairpin:   &yes, PortPreserving: &no,
		},
		PortMap: PortMapReport{
			Status:  StatusOK,
			Gateway: netip.MustParseAddr("192.168.1.1"),
			PCP:     ServiceProbe{Available: true},
		},
		Overseas: OverseasReport{
			Status: StatusOK,
			Probes: []ReachProbe{{URL: "https://example/204", OK: true}},
		},
		Egress: EgressReport{
			Status:    StatusOK,
			UniqueIPs: []netip.Addr{netip.MustParseAddr("1.2.3.4")},
		},
		Tailscale: TailscaleReport{Status: StatusOK, Available: true, PreferredDERP: "tok"},
	}
}

// TestHeadlineRouterWithoutPortMapIsNotAFailure is the regression this logic
// exists for. A router that simply does not implement UPnP/NAT-PMP/PCP is a
// measured fact, not a broken run — but because Report.Status is the worst of
// every section, that one warn used to drag the whole report into "存在若干需要
// 注意的项目" in alarm colours.
func TestHeadlineRouterWithoutPortMapIsNotAFailure(t *testing.T) {
	r := measuredReport()
	r.PortMap.Status = StatusWarn
	r.PortMap.PCP = ServiceProbe{Err: "timeout"}
	r.PortMap.Summary = "路由器未响应 UPnP / NAT-PMP / PCP"
	r.Status = worstStatus(r.Interfaces.Status, r.UDP.Status, r.NAT.Status,
		r.PortMap.Status, r.Overseas.Status, r.Egress.Status, r.Tailscale.Status)

	if r.Status != StatusWarn {
		t.Fatalf("precondition: report status = %v, want warn", r.Status)
	}
	text, status := headline(r)
	if status != StatusOK {
		t.Errorf("headline status = %v, want ok — every section produced a result", status)
	}
	if strings.Contains(text, "需要注意") {
		t.Errorf("headline = %q, must not call a fully measured run 'needs attention'", text)
	}
}

// TestHeadlineSevereFindingsOutrankIndeterminacy: when UDP is blocked the NAT
// section genuinely cannot be classified, but "UDP 被完全阻断" is the useful
// sentence — naming NAT as undetermined would bury the cause under a symptom.
func TestHeadlineSevereFindingsOutrankIndeterminacy(t *testing.T) {
	r := measuredReport()
	r.NAT.Type = NATUDPBlocked
	r.NAT.Mapping, r.NAT.Filtering = BehaviorUnknown, BehaviorUnknown
	r.NAT.Status = StatusFail
	r.UDP.V4OK, r.UDP.V6OK = false, false

	text, status := headline(r)
	if status != StatusFail {
		t.Errorf("status = %v, want fail", status)
	}
	if !strings.Contains(text, "UDP 被完全阻断") {
		t.Errorf("headline = %q, want the UDP-blocked sentence", text)
	}
}

func TestHeadlineAllOK(t *testing.T) {
	text, status := headline(measuredReport())
	if status != StatusOK {
		t.Errorf("status = %v, want ok", status)
	}
	if !strings.Contains(text, "网络状况良好") {
		t.Errorf("headline = %q", text)
	}
}

// TestHeadlineIndeterminate covers the other side: a section that could not
// measure anything is what actually deserves the cautionary headline.
func TestHeadlineIndeterminate(t *testing.T) {
	r := measuredReport()
	r.NAT.Filtering = BehaviorUnknown
	// Keep every section's own status OK so the only signal is indeterminacy.
	text, status := headline(r)
	if status != StatusWarn {
		t.Errorf("status = %v, want warn", status)
	}
	if !strings.Contains(text, "NAT 类型") {
		t.Errorf("headline = %q, should name the section that failed to measure", text)
	}
}

func TestIndeterminate(t *testing.T) {
	if got := indeterminate(measuredReport()); len(got) != 0 {
		t.Fatalf("healthy report has indeterminate sections: %v", got)
	}

	cases := []struct {
		name  string
		spoil func(*Report)
		want  string
	}{
		{"interfaces failed", func(r *Report) { r.Interfaces.Err = "no permission" }, "本机地址"},
		{"no udp probes ran", func(r *Report) { r.UDP.Probes = nil }, "UDP 连通性"},
		{"nat undetermined", func(r *Report) { r.NAT.Type = NATUnknown }, "NAT 类型"},
		{"filtering undetermined", func(r *Report) { r.NAT.Filtering = BehaviorUnknown }, "NAT 类型"},
		{"no gateway found", func(r *Report) { r.PortMap.Gateway = netip.Addr{} }, "端口映射"},
		{"overseas skipped", func(r *Report) { r.Overseas.Status = StatusSkipped }, "境外连通性"},
		{"no egress ip", func(r *Report) { r.Egress.UniqueIPs = nil }, "出口 IP"},
		{"netcheck attempted and failed", func(r *Report) {
			r.Tailscale.Status = StatusSkipped
			r.Tailscale.Err = "no DERP map available"
		}, "Tailscale 内部状态"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := measuredReport()
			tc.spoil(r)
			got := indeterminate(r)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("indeterminate = %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestIndeterminateIgnoresBadButMeasuredResults pins the core distinction: an
// unwelcome answer is still an answer.
func TestIndeterminateIgnoresBadButMeasuredResults(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(*Report)
	}{
		{"symmetric nat", func(r *Report) {
			r.NAT.Type = NATSymmetric
			r.NAT.Mapping = BehaviorAddressAndPortDependent
		}},
		{"router has no port mapping", func(r *Report) {
			r.PortMap.Status = StatusWarn
			r.PortMap.PCP = ServiceProbe{Err: "timeout"}
		}},
		{"overseas unreachable", func(r *Report) {
			r.Overseas.Status = StatusFail
			r.Overseas.Probes = []ReachProbe{{URL: "https://example/204", Err: "timeout"}}
		}},
		// No tailscale source configured: the caller opted out, exactly as with
		// SkipGeo. Nothing failed.
		{"tailscale not wired up", func(r *Report) {
			r.Tailscale = TailscaleReport{
				Status:  StatusSkipped,
				Summary: "Tailscale 未运行，跳过内部状态检查",
			}
		}},
		{"udp probes all failed", func(r *Report) {
			r.UDP.V4OK, r.UDP.V6OK = false, false
			r.UDP.Probes = []UDPProbe{{Host: "stun.example:3478", Err: "timeout"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := measuredReport()
			tc.spoil(r)
			if got := indeterminate(r); len(got) != 0 {
				t.Fatalf("indeterminate = %v, want none — a bad result is still a result", got)
			}
		})
	}
}
