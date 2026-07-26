// Package netdiag runs network diagnostics: NAT classification via STUN, UDP
// reachability, local address enumeration, router port-mapping support
// (UPnP/NAT-PMP/PCP), overseas reachability, and public egress IP discovery
// with geolocation.
//
// The package deliberately avoids depending on tailscale.com so it stays
// usable (and testable) on its own. Tailscale's own view of the network is
// injected through the [TailscaleSource] interface.
package netdiag

import (
	"context"
	"log/slog"
	"net/netip"
	"time"
)

// Status is a coarse traffic-light verdict attached to each section of a
// [Report] so the UI can rank what deserves the user's attention.
type Status int

const (
	StatusUnknown Status = iota
	StatusOK
	StatusWarn
	StatusFail
	StatusSkipped
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

// Region distinguishes probe targets inside mainland China from targets
// outside it. Egress results routinely differ between the two when a proxy is
// in play, and that difference is itself a diagnostic signal.
type Region string

const (
	RegionCN   Region = "cn"
	RegionIntl Region = "intl"
)

func (r Region) String() string { return string(r) }

// ---------------------------------------------------------------------------
// Local addresses
// ---------------------------------------------------------------------------

// AddrKind classifies a local address by the scope it can reach.
type AddrKind string

const (
	AddrGlobalV4  AddrKind = "global4"
	AddrPrivateV4 AddrKind = "private4"
	AddrCGNAT     AddrKind = "cgnat"
	AddrGlobalV6  AddrKind = "global6"
	AddrULA       AddrKind = "ula"
	AddrLinkLocal AddrKind = "link-local"
	AddrLoopback  AddrKind = "loopback"
	AddrTailscale AddrKind = "tailscale"
)

// LocalAddr is one address bound to one local interface.
type LocalAddr struct {
	Iface    string
	Addr     netip.Addr
	Prefix   netip.Prefix
	Kind     AddrKind
	Up       bool
	MTU      int
	Hardware string // MAC, empty for virtual interfaces
	// IsDefaultSrc reports whether the kernel picks this address as the source
	// for a default-route destination.
	IsDefaultSrc bool
}

// InterfaceReport enumerates every local address, so the user can see all
// IPv4/IPv6 exits the machine has.
type InterfaceReport struct {
	Addrs        []LocalAddr
	DefaultV4Src netip.Addr
	DefaultV6Src netip.Addr
	HasGlobalV6  bool
	Status       Status
	Summary      string
	Err          string
}

// ---------------------------------------------------------------------------
// STUN / UDP / NAT
// ---------------------------------------------------------------------------

// STUNServer is one probe target.
type STUNServer struct {
	Host   string // "stun.miwifi.com:3478"
	Name   string // human label, e.g. "小米"
	Region Region
}

// STUNResult records the outcome of a single binding transaction.
type STUNResult struct {
	Server string
	Name   string
	Region Region
	OK     bool
	RTT    time.Duration
	// Mapped is the server-reflexive address the server saw.
	Mapped netip.AddrPort
	// Other is the OTHER-ADDRESS (RFC 5780) or CHANGED-ADDRESS (RFC 3489)
	// alternate transport address, when advertised.
	Other netip.AddrPort
	// SupportsChangeReq reports whether the server honoured a CHANGE-REQUEST,
	// which is required for filtering-behaviour discovery.
	SupportsChangeReq bool
	Software          string
	Err               string
}

// UDPProbe is a plain "can I send and receive UDP here" datapoint.
type UDPProbe struct {
	// Host is the configured "hostname:port", kept alongside the resolved
	// Target so the UI can name the server rather than an anonymous address.
	Host string
	// Target is the address actually probed, "ip:port". A server reachable over
	// both families yields one probe per family, and only this tells them apart.
	Target string
	Name   string
	Region Region
	Port   int
	OK     bool
	RTT    time.Duration
	Mapped netip.AddrPort
	Err    string
}

// UDPReport summarises UDP reachability across regions and ports.
type UDPReport struct {
	V4OK    bool
	V6OK    bool
	Probes  []UDPProbe
	OKPorts []int
	// BlockedPorts are ports where every probe failed while some other port
	// succeeded — a strong hint of egress filtering rather than no UDP at all.
	BlockedPorts []int
	CNReachable  int
	CNTotal      int
	IntlReachabl int
	IntlTotal    int
	Status       Status
	Summary      string
}

// Behavior is the RFC 5780 mapping/filtering behaviour classification.
type Behavior int

const (
	BehaviorUnknown Behavior = iota
	BehaviorEndpointIndependent
	BehaviorAddressDependent
	BehaviorAddressAndPortDependent
)

func (b Behavior) String() string {
	switch b {
	case BehaviorEndpointIndependent:
		return "endpoint-independent"
	case BehaviorAddressDependent:
		return "address-dependent"
	case BehaviorAddressAndPortDependent:
		return "address-and-port-dependent"
	default:
		return "unknown"
	}
}

// NATType is the classic RFC 3489 name for the detected NAT, kept because it
// is what users recognise (and what game/P2P docs talk about).
type NATType string

const (
	NATUnknown      NATType = "unknown"
	NATOpen         NATType = "open"       // no NAT, reflexive == local
	NATFullCone     NATType = "full-cone"  // NAT type 1-ish
	NATRestricted   NATType = "restricted" // address-restricted cone
	NATPortRestrict NATType = "port-restricted"
	NATSymmetric    NATType = "symmetric" // worst case for P2P
	NATUDPBlocked   NATType = "udp-blocked"
	NATSymmetricFW  NATType = "symmetric-firewall" // no NAT but stateful firewall
)

// NATReport is the NAT classification result.
type NATReport struct {
	Type      NATType
	Mapping   Behavior
	Filtering Behavior
	// Hairpin reports whether the NAT loops packets sent to its own external
	// address back inside. nil when untested.
	Hairpin *bool
	// PortPreserving reports whether the external port equals the local port.
	PortPreserving *bool
	// MappedAddrs is every distinct reflexive address observed. More than one
	// means the mapping varies by destination (symmetric).
	MappedAddrs []netip.AddrPort
	Results     []STUNResult
	Status      Status
	Summary     string
	Notes       []string
}

// ---------------------------------------------------------------------------
// Router port mapping
// ---------------------------------------------------------------------------

// ServiceProbe is the result of probing one port-mapping protocol.
type ServiceProbe struct {
	Available  bool
	Detail     string // device name / protocol version / control URL
	ExternalIP netip.Addr
	RTT        time.Duration
	Err        string
}

// PortMapReport covers UPnP IGD, NAT-PMP and PCP.
type PortMapReport struct {
	Gateway netip.Addr
	UPnP    ServiceProbe
	NATPMP  ServiceProbe
	PCP     ServiceProbe
	Status  Status
	Summary string
}

// ---------------------------------------------------------------------------
// Reachability
// ---------------------------------------------------------------------------

// ReachProbe is one HTTP/TCP reachability datapoint.
type ReachProbe struct {
	Name       string
	URL        string
	Region     Region
	OK         bool
	StatusCode int
	RTT        time.Duration
	// ViaProxy reports whether the request honoured the environment's proxy
	// settings. Running the same target both ways reveals proxy interference.
	ViaProxy bool
	Network  string // "tcp4", "tcp6" or "" for unforced
	Err      string
}

// OverseasReport captures whether traffic can leave for the wider internet,
// primarily via cp.cloudflare.com.
type OverseasReport struct {
	Probes  []ReachProbe
	Status  Status
	Summary string
}

// ---------------------------------------------------------------------------
// Egress IP + geolocation
// ---------------------------------------------------------------------------

// EgressMethod is how a public address was observed. Different methods take
// different paths out of the machine, so they legitimately disagree when a
// proxy or split tunnel is active.
type EgressMethod string

const (
	MethodSTUN      EgressMethod = "stun"       // raw UDP, bypasses HTTP proxies
	MethodHTTPv4    EgressMethod = "http4"      // forced IPv4, proxy bypassed
	MethodHTTPv6    EgressMethod = "http6"      // forced IPv6, proxy bypassed
	MethodHTTPProxy EgressMethod = "http-proxy" // honours HTTP(S)_PROXY
	MethodTailscale EgressMethod = "tailscale"  // as seen by the tailnet
)

// EgressObservation is one "what is my public IP" answer.
type EgressObservation struct {
	Method EgressMethod
	Source string // server or URL that answered
	Region Region
	IP     netip.Addr
	RTT    time.Duration
	Err    string
}

// GeoInfo is the geolocation of one public IP.
type GeoInfo struct {
	IP          netip.Addr
	Country     string // ISO code
	CountryName string
	Region      string
	City        string
	Org         string
	ASN         string
	Loc         string
	Timezone    string
	Provider    string // which API answered
	Err         string
}

// EgressReport lists every public address the machine appears to use.
type EgressReport struct {
	Observations []EgressObservation
	Geo          []GeoInfo
	// UniqueIPs is the deduplicated set across all methods.
	UniqueIPs []netip.Addr
	// Divergent is true when the probes disagreed about our public address
	// within one address family, which usually means a proxy or VPN is
	// intercepting part of the traffic. Having both an IPv4 and an IPv6 egress
	// is ordinary dual stack and does not set this.
	Divergent bool
	// DivergentSTUN narrows Divergent to the case that actually breaks NAT
	// traversal: STUN itself — plain UDP, the same path Tailscale punches
	// through — saw more than one address in a family. That means the UDP
	// egress genuinely varies per flow.
	//
	// Divergence seen only by the HTTP probes is a weaker signal. An HTTP proxy
	// or split-tunnel rule can rewrite web traffic while leaving UDP alone, so
	// it warrants a warning, not a verdict.
	DivergentSTUN bool
	// Countries is the set of distinct countries seen, sorted.
	Countries []string
	Status    Status
	Summary   string
}

// ---------------------------------------------------------------------------
// Tailscale's own view
// ---------------------------------------------------------------------------

// DERPLatency is the round-trip time to one DERP region.
type DERPLatency struct {
	RegionID   int
	RegionCode string
	Name       string
	Latency    time.Duration
	Preferred  bool
}

// TailscaleReport mirrors the parts of tailscale's netcheck report that are
// useful here. Tri-state fields are nil when tailscale could not determine
// them.
type TailscaleReport struct {
	Available             bool
	UDP                   bool
	IPv4                  bool
	IPv6                  bool
	ICMPv4                bool
	OSHasIPv6             bool
	MappingVariesByDestIP *bool
	UPnP                  *bool
	PMP                   *bool
	PCP                   *bool
	CaptivePortal         *bool
	GlobalV4              string
	GlobalV6              string
	PreferredDERP         string
	DERP                  []DERPLatency
	Status                Status
	Summary               string
	Err                   string
}

// TailscaleSource supplies tailscale's internal network view. The GUI wires
// this to a live tsnet server; it is nil when tailscale is not running yet.
type TailscaleSource interface {
	Netcheck(ctx context.Context) (*TailscaleReport, error)
}

// ---------------------------------------------------------------------------
// Report + runner
// ---------------------------------------------------------------------------

// Report is the complete diagnostic result.
type Report struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Duration   time.Duration

	Interfaces InterfaceReport
	UDP        UDPReport
	NAT        NATReport
	PortMap    PortMapReport
	Overseas   OverseasReport
	Egress     EgressReport
	Tailscale  TailscaleReport

	// Headline is the single most important sentence about this report.
	Headline string
	// HeadlineStatus is the severity of Headline specifically, which is not
	// always Status. Status is the worst of every section, so a report with an
	// unrelated failure elsewhere would otherwise paint a merely-cautionary
	// headline in alarm red and overstate what was actually found.
	HeadlineStatus Status
	// Status is the worst status across all sections.
	Status Status
}

// Step identifies one unit of diagnostic work. The GUI renders these as a
// checklist while the run is in flight.
type Step struct {
	Key   string
	Title string
}

// Steps lists every phase in execution order.
var Steps = []Step{
	{Key: "iface", Title: "本机网络接口"},
	{Key: "udp", Title: "UDP 连通性"},
	{Key: "nat", Title: "NAT 类型"},
	{Key: "portmap", Title: "UPnP / NAT-PMP / PCP"},
	{Key: "overseas", Title: "境外连通性"},
	{Key: "egress", Title: "出口 IP"},
	{Key: "geo", Title: "IP 归属地"},
	{Key: "tailscale", Title: "Tailscale 内部状态"},
}

// Progress is emitted as each step starts and finishes.
type Progress struct {
	Key     string
	Title   string
	Index   int
	Total   int
	Done    bool
	Err     string
	Elapsed time.Duration
}

// Options configures a diagnostic run.
type Options struct {
	Logger *slog.Logger
	// OnProgress is called from the runner's goroutines; implementations must
	// be safe for concurrent use.
	OnProgress func(Progress)
	// Tailscale is optional; when nil the tailscale section is skipped.
	Tailscale TailscaleSource
	// STUNServers overrides the default CN + international server list.
	STUNServers []STUNServer
	// IPInfoToken is an optional ipinfo.io token, raising the rate limit.
	IPInfoToken string
	// Timeout bounds the whole run. Zero means DefaultTimeout.
	Timeout time.Duration
	// SkipGeo disables outbound geolocation lookups (they leak the user's IP
	// to a third party).
	SkipGeo bool
}

// DefaultTimeout bounds a full diagnostic run.
const DefaultTimeout = 45 * time.Second

func (o *Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

func (o *Options) progress(p Progress) {
	if o.OnProgress != nil {
		o.OnProgress(p)
	}
}

// worstStatus returns the most severe status in ss, treating StatusSkipped and
// StatusUnknown as less severe than StatusWarn.
func worstStatus(ss ...Status) Status {
	rank := map[Status]int{
		StatusOK:      0,
		StatusSkipped: 1,
		StatusUnknown: 2,
		StatusWarn:    3,
		StatusFail:    4,
	}
	worst := StatusOK
	for _, s := range ss {
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	return worst
}

func boolPtr(b bool) *bool { return &b }
