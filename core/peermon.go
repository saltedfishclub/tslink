package core

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// PeerRoute is how traffic currently reaches a peer.
type PeerRoute string

const (
	RouteDirect    PeerRoute = "direct"
	RouteDERP      PeerRoute = "derp"
	RoutePeerRelay PeerRoute = "peer-relay"
	RouteOffline   PeerRoute = "offline"
	RouteUnknown   PeerRoute = "unknown"
)

// PeerSample is one latency measurement.
type PeerSample struct {
	At      time.Time
	Latency time.Duration
	OK      bool
	Route   PeerRoute
}

// PeerInfo is everything the GUI shows about one node.
type PeerInfo struct {
	ID, HostName, DNSName, DisplayName, OS      string
	TailscaleIPs                                []netip.Addr
	Online, Active, ExitNode                    bool
	CurAddr, Relay                              string
	Route                                       PeerRoute
	RxBytes, TxBytes                            int64
	Created, LastSeen, LastWrite, LastHandshake time.Time
	// Linked reports that a config rule points at this peer; those are the nodes
	// the user actually cares about and the GUI lists them first.
	Linked      bool
	LinkTags    []string
	LastLatency time.Duration
	LatencyOK   bool
	Samples     []PeerSample // chronological, oldest first
	AvgLatency  time.Duration
	MinLatency  time.Duration
	MaxLatency  time.Duration
	JitterMs    float64 // mean absolute successive difference
	LossPct     float64
}

// clone returns a deep copy of p so callers cannot reach into monitor state.
func (p PeerInfo) clone() PeerInfo {
	out := p
	out.TailscaleIPs = append([]netip.Addr(nil), p.TailscaleIPs...)
	out.LinkTags = append([]string(nil), p.LinkTags...)
	out.Samples = append([]PeerSample(nil), p.Samples...)
	return out
}

// PeerSnapshot is a consistent view of the tailnet at one instant.
type PeerSnapshot struct {
	At             time.Time
	Valid          bool
	Self           PeerInfo
	Peers          []PeerInfo
	TailnetName    string
	BackendState   string
	MagicDNSSuffix string
	Err            string
}

// clone returns a deep copy of s, including every peer's slices.
func (s PeerSnapshot) clone() PeerSnapshot {
	out := s
	out.Self = s.Self.clone()
	out.Peers = make([]PeerInfo, len(s.Peers))
	for i, p := range s.Peers {
		out.Peers[i] = p.clone()
	}
	return out
}

// PeerMonitorOptions tunes the two polling loops and the history depth.
type PeerMonitorOptions struct {
	// StatusInterval defaults to 3s, PingInterval to 10s, HistorySize to 120 samples.
	StatusInterval, PingInterval time.Duration
	HistorySize                  int
}

const (
	defaultStatusInterval = 3 * time.Second
	defaultPingInterval   = 10 * time.Second
	defaultHistorySize    = 120

	// pingTimeout bounds a single peer ping. A hung probe must never stall the
	// sweep, and the sweep must never outlive its own interval by much.
	pingTimeout = 5 * time.Second
	// pingConcurrency bounds in-flight pings so a large tailnet cannot spawn
	// hundreds of goroutines at once.
	pingConcurrency = 4
	// linkResolveInterval re-resolves config rules, because MagicDNS answers
	// change when a peer's address is reassigned.
	linkResolveInterval = 5 * time.Minute
	// linkResolveTimeout bounds resolution of a single rule destination.
	linkResolveTimeout = 10 * time.Second
	// statusTimeout bounds one lc.Status call.
	statusTimeout = 10 * time.Second
	// maxStatusBackoff caps the retry delay after repeated status failures.
	maxStatusBackoff = 30 * time.Second
)

func (o PeerMonitorOptions) withDefaults() PeerMonitorOptions {
	if o.StatusInterval <= 0 {
		o.StatusInterval = defaultStatusInterval
	}
	if o.PingInterval <= 0 {
		o.PingInterval = defaultPingInterval
	}
	if o.HistorySize <= 0 {
		o.HistorySize = defaultHistorySize
	}
	return o
}

// pingOutcome is the most recent ping result for one peer, used to refine the
// route derivation that the status fields alone can only guess at.
type pingOutcome struct {
	ok         bool
	latency    time.Duration
	derpRegion string
	at         time.Time
}

// PeerMonitor keeps a live view of the tailnet for the GUI: a cheap status
// poll, an independent ping sweep, and a capped latency history per peer.
//
// All methods are safe for concurrent use.
type PeerMonitor struct {
	srv   *tsnet.Server
	rules map[string][]ConnectRule
	log   *slog.Logger
	opt   PeerMonitorOptions

	refreshStatus chan struct{}
	refreshPing   chan struct{}
	refreshLinks  chan struct{}

	mu      sync.RWMutex
	raw     *ipnstate.Status // last good status, nil until the first poll lands
	rawErr  string
	built   PeerSnapshot // rebuilt after every poll and sweep
	hist    map[string][]PeerSample
	last    map[string]pingOutcome
	links   map[netip.Addr][]string
	subs    map[int]chan struct{}
	nextSub int
}

// NewPeerMonitor returns a monitor for srv. rules are the configured connect
// rules, used to mark which peers the user actually links to; it may be nil.
// logger may be nil.
func NewPeerMonitor(srv *tsnet.Server, rules map[string][]ConnectRule, logger *slog.Logger, opt PeerMonitorOptions) *PeerMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &PeerMonitor{
		srv:           srv,
		rules:         rules,
		log:           logger.With("from", "peermon"),
		opt:           opt.withDefaults(),
		refreshStatus: make(chan struct{}, 1),
		refreshPing:   make(chan struct{}, 1),
		refreshLinks:  make(chan struct{}, 1),
		hist:          make(map[string][]PeerSample),
		last:          make(map[string]pingOutcome),
		links:         make(map[netip.Addr][]string),
		subs:          make(map[int]chan struct{}),
	}
}

// Start launches the status loop, the ping loop and the link resolver. All of
// them stop when ctx is cancelled. Start does not block.
func (m *PeerMonitor) Start(ctx context.Context) {
	go m.statusLoop(ctx)
	go m.pingLoop(ctx)
	go m.linkLoop(ctx)
}

// RefreshNow triggers an immediate status+ping cycle without blocking the caller.
//
// Link resolution is kicked too. It normally runs every linkResolveInterval,
// but the GUI now lists only linked peers, so a user staring at an empty page
// after a DNS hiccup has no other way to ask for a retry.
func (m *PeerMonitor) RefreshNow() {
	kick(m.refreshStatus)
	kick(m.refreshPing)
	kick(m.refreshLinks)
}

// kick delivers a coalescing wakeup: a pending signal is enough.
func kick(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Snapshot returns a consistent, fully copied view of the tailnet. It performs
// no I/O and is safe to call from the render path.
func (m *PeerMonitor) Snapshot() PeerSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.built.clone()
}

// Subscribe returns a channel that receives a value after every status poll and
// every completed ping sweep, plus a function that cancels the subscription.
// The channel is buffered and coalescing: a slow reader sees one wakeup, not a
// backlog.
func (m *PeerMonitor) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = ch
	m.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.subs, id)
			m.mu.Unlock()
		})
	}
	return ch, cancel
}

// History returns the samples for one peer keyed by stable node ID, oldest
// first. The returned slice is a copy.
func (m *PeerMonitor) History(id string) []PeerSample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]PeerSample(nil), m.hist[id]...)
}

func (m *PeerMonitor) notify() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default: // subscriber has a pending wakeup already
		}
	}
}

// ---------------------------------------------------------------------------
// status loop
// ---------------------------------------------------------------------------

// statusLoop polls lc.Status on StatusInterval. It never waits on the ping
// sweep, so a slow tailnet cannot freeze the peer list in the GUI.
func (m *PeerMonitor) statusLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	var fails int
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-m.refreshStatus:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}

		err := m.pollStatus(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := m.opt.StatusInterval
		if err != nil {
			fails++
			delay = backoffDelay(m.opt.StatusInterval, fails)
			m.log.Debug("status poll failed", "err", err, "retry_in", delay)
		} else {
			fails = 0
			if first {
				first = false
				kick(m.refreshPing) // ping as soon as we know who is out there
			}
		}
		timer.Reset(delay)
	}
}

// backoffDelay grows the retry delay exponentially, capped at maxStatusBackoff.
func backoffDelay(base time.Duration, fails int) time.Duration {
	d := base
	for i := 1; i < fails && d < maxStatusBackoff; i++ {
		d *= 2
	}
	if d > maxStatusBackoff {
		d = maxStatusBackoff
	}
	return d
}

// pollStatus refreshes the cached status. On failure the previous status is
// kept so the GUI degrades to stale data instead of going blank.
func (m *PeerMonitor) pollStatus(ctx context.Context) error {
	lc, err := m.localClient()
	if err == nil {
		var st *ipnstate.Status
		st, err = func() (*ipnstate.Status, error) {
			cctx, cancel := context.WithTimeout(ctx, statusTimeout)
			defer cancel()
			return lc.Status(cctx)
		}()
		if err == nil {
			m.mu.Lock()
			m.raw = st
			m.rawErr = ""
			m.rebuildLocked()
			m.mu.Unlock()
			m.notify()
			return nil
		}
	}

	m.mu.Lock()
	m.rawErr = err.Error()
	m.rebuildLocked()
	m.mu.Unlock()
	m.notify()
	return err
}

func (m *PeerMonitor) localClient() (*local.Client, error) {
	if m.srv == nil {
		return nil, errors.New("tsnet server not started")
	}
	return m.srv.LocalClient()
}

// ---------------------------------------------------------------------------
// ping loop
// ---------------------------------------------------------------------------

// pingLoop sweeps every online peer on PingInterval. A sweep that overruns its
// interval simply delays the next sweep; the status loop is unaffected.
func (m *PeerMonitor) pingLoop(ctx context.Context) {
	timer := time.NewTimer(m.opt.PingInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-m.refreshPing:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}

		m.pingSweep(ctx)
		if ctx.Err() != nil {
			return
		}
		timer.Reset(m.opt.PingInterval)
	}
}

// pingTarget is one node to probe in a sweep.
type pingTarget struct {
	id   string
	name string
	addr netip.Addr
}

// pingTargets lists the online peers worth probing, taken from the last good
// status. Self is skipped: pinging your own address is not a network test.
func (m *PeerMonitor) pingTargets() []pingTarget {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.raw == nil {
		return nil
	}
	var out []pingTarget
	for _, ps := range m.raw.Peer {
		if ps == nil || !ps.Online {
			continue
		}
		addr := pingAddr(ps.TailscaleIPs)
		if !addr.IsValid() {
			continue
		}
		out = append(out, pingTarget{id: peerKey(ps), name: displayName(ps), addr: addr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// pingSweep probes every online peer, bounded to pingConcurrency in flight.
func (m *PeerMonitor) pingSweep(ctx context.Context) {
	targets := m.pingTargets()
	if len(targets) == 0 {
		return
	}
	lc, err := m.localClient()
	if err != nil {
		m.log.Debug("ping sweep skipped", "err", err)
		return
	}

	sem := make(chan struct{}, pingConcurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(t pingTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			m.pingOne(ctx, lc, t)
		}(t)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	m.mu.Lock()
	m.rebuildLocked()
	m.mu.Unlock()
	m.notify()
}

// pingOne probes a single peer and records the outcome. A failure is recorded
// as a sample with OK=false: loss is data.
func (m *PeerMonitor) pingOne(ctx context.Context, lc *local.Client, t pingTarget) {
	cctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	res, err := lc.Ping(cctx, t.addr, tailcfg.PingDisco)
	now := time.Now()

	out := pingOutcome{at: now}
	switch {
	case err != nil:
		if !errors.Is(err, context.Canceled) {
			m.log.Debug("peer ping failed", "peer", t.name, "addr", t.addr, "err", err)
		}
	case res == nil:
		m.log.Debug("peer ping returned nothing", "peer", t.name, "addr", t.addr)
	case res.Err != "":
		m.log.Debug("peer ping error", "peer", t.name, "addr", t.addr, "err", res.Err)
	default:
		out.ok = true
		out.latency = time.Duration(res.LatencySeconds * float64(time.Second))
		out.derpRegion = res.DERPRegionCode
	}

	sample := PeerSample{At: now, Latency: out.latency, OK: out.ok}
	if out.ok {
		if out.derpRegion == "" {
			sample.Route = RouteDirect
		} else {
			sample.Route = RouteDERP
		}
	} else {
		sample.Route = RouteUnknown
	}

	m.mu.Lock()
	m.last[t.id] = out
	m.hist[t.id] = appendSample(m.hist[t.id], sample, m.opt.HistorySize)
	m.mu.Unlock()
}

// appendSample pushes s onto a capped ring, dropping the oldest entry when
// full. Chronological order is preserved.
func appendSample(ring []PeerSample, s PeerSample, size int) []PeerSample {
	if size <= 0 {
		size = defaultHistorySize
	}
	if len(ring) < size {
		return append(ring, s)
	}
	// Shift left by the overflow so a shrunken HistorySize also converges.
	drop := len(ring) - size + 1
	copy(ring, ring[drop:])
	ring = ring[:size-1]
	return append(ring, s)
}

// ---------------------------------------------------------------------------
// link resolution
// ---------------------------------------------------------------------------

// linkLoop resolves every connect rule's destination to a tailnet address once
// at start and again every linkResolveInterval. Resolution touches the network,
// so it never happens on the render path.
func (m *PeerMonitor) linkLoop(ctx context.Context) {
	if len(m.rules) == 0 || m.srv == nil {
		return
	}
	ticker := time.NewTicker(linkResolveInterval)
	defer ticker.Stop()

	m.resolveLinks(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.resolveLinks(ctx)
		case <-m.refreshLinks:
			m.resolveLinks(ctx)
		}
	}
}

// resolveLinks maps every rule destination to a peer address, remembering which
// config tags referenced it.
func (m *PeerMonitor) resolveLinks(ctx context.Context) {
	found := make(map[netip.Addr]map[string]struct{})

	for tag, rules := range m.rules {
		for _, rule := range rules {
			if ctx.Err() != nil {
				return
			}
			host, _, err := net.SplitHostPort(rule.DstAddr)
			if err != nil {
				m.log.Debug("link: bad dst_addr", "tag", tag, "dst", rule.DstAddr, "err", err)
				continue
			}
			addr, err := func() (*netip.Addr, error) {
				cctx, cancel := context.WithTimeout(ctx, linkResolveTimeout)
				defer cancel()
				return resolveAddr(cctx, m.srv, host)
			}()
			if err != nil || addr == nil {
				m.log.Debug("link: failed to resolve dst_addr", "tag", tag, "dst", rule.DstAddr, "err", err)
				continue
			}
			if found[*addr] == nil {
				found[*addr] = make(map[string]struct{})
			}
			found[*addr][tag] = struct{}{}
		}
	}

	if ctx.Err() != nil {
		return
	}

	links := make(map[netip.Addr][]string, len(found))
	for addr, tags := range found {
		list := make([]string, 0, len(tags))
		for tag := range tags {
			list = append(list, tag)
		}
		sort.Strings(list)
		links[addr] = list
	}

	m.mu.Lock()
	m.links = links
	m.rebuildLocked()
	m.mu.Unlock()
	m.log.Debug("link targets resolved", "count", len(links))
	m.notify()
}

// ---------------------------------------------------------------------------
// snapshot assembly
// ---------------------------------------------------------------------------

// rebuildLocked recomputes the cached snapshot from the last good status, the
// latency history and the resolved links. m.mu must be held for writing.
func (m *PeerMonitor) rebuildLocked() {
	snap := PeerSnapshot{At: time.Now(), Err: m.rawErr}
	st := m.raw
	if st == nil {
		snap.Valid = false
		m.built = snap
		return
	}

	// Stale data is still useful data: Valid stays true once a status landed,
	// and Err tells the GUI the view may be out of date.
	snap.Valid = true
	snap.BackendState = st.BackendState
	snap.MagicDNSSuffix = st.MagicDNSSuffix
	if st.CurrentTailnet != nil {
		snap.TailnetName = st.CurrentTailnet.Name
		if st.CurrentTailnet.MagicDNSSuffix != "" {
			snap.MagicDNSSuffix = st.CurrentTailnet.MagicDNSSuffix
		}
	}

	live := make(map[string]struct{}, len(st.Peer)+1)
	if st.Self != nil {
		snap.Self = m.peerInfoLocked(st.Self)
		live[snap.Self.ID] = struct{}{}
	}
	snap.Peers = make([]PeerInfo, 0, len(st.Peer))
	for _, ps := range st.Peer {
		if ps == nil {
			continue
		}
		info := m.peerInfoLocked(ps)
		live[info.ID] = struct{}{}
		snap.Peers = append(snap.Peers, info)
	}
	sortPeers(snap.Peers)

	// Forget history for nodes that left the netmap, so a long-running GUI
	// session does not grow without bound.
	for id := range m.hist {
		if _, ok := live[id]; !ok {
			delete(m.hist, id)
			delete(m.last, id)
		}
	}

	m.built = snap
}

// peerInfoLocked converts one PeerStatus into the GUI's view of it. m.mu must
// be held.
func (m *PeerMonitor) peerInfoLocked(ps *ipnstate.PeerStatus) PeerInfo {
	id := peerKey(ps)
	info := PeerInfo{
		ID:            id,
		HostName:      ps.HostName,
		DNSName:       strings.TrimSuffix(ps.DNSName, "."),
		DisplayName:   displayName(ps),
		OS:            ps.OS,
		TailscaleIPs:  append([]netip.Addr(nil), ps.TailscaleIPs...),
		Online:        ps.Online,
		Active:        ps.Active,
		ExitNode:      ps.ExitNode,
		CurAddr:       ps.CurAddr,
		Relay:         ps.Relay,
		RxBytes:       ps.RxBytes,
		TxBytes:       ps.TxBytes,
		Created:       ps.Created,
		LastSeen:      ps.LastSeen,
		LastWrite:     ps.LastWrite,
		LastHandshake: ps.LastHandshake,
	}

	for _, ip := range ps.TailscaleIPs {
		tags, ok := m.links[ip]
		if !ok {
			continue
		}
		info.Linked = true
		info.LinkTags = mergeTags(info.LinkTags, tags)
	}

	last, hasPing := m.last[id]
	info.Route = deriveRoute(ps, last, hasPing)
	if hasPing {
		info.LatencyOK = last.ok
		if last.ok {
			info.LastLatency = last.latency
		}
	}

	samples := m.hist[id]
	info.Samples = append([]PeerSample(nil), samples...)
	summariseSamples(&info)
	return info
}

// deriveRoute decides how traffic reaches the peer. Status fields give the
// baseline; a successful ping is authoritative because it reports the path the
// packet actually took.
func deriveRoute(ps *ipnstate.PeerStatus, last pingOutcome, hasPing bool) PeerRoute {
	if hasPing && last.ok {
		if last.derpRegion != "" {
			return RouteDERP
		}
		if ps.PeerRelay != "" {
			return RoutePeerRelay
		}
		return RouteDirect
	}
	switch {
	case ps.PeerRelay != "":
		return RoutePeerRelay
	case ps.CurAddr != "":
		return RouteDirect
	case ps.Relay != "":
		return RouteDERP
	case !ps.Online:
		return RouteOffline
	default:
		return RouteUnknown
	}
}

// summariseSamples fills the aggregate latency fields. Averages, minimum,
// maximum and jitter consider successful samples only; loss covers the whole
// window.
func summariseSamples(info *PeerInfo) {
	if len(info.Samples) == 0 {
		return
	}
	var (
		sum      time.Duration
		ok       int
		fails    int
		lo, hi   time.Duration
		prev     time.Duration
		havePrev bool
		diffSum  float64
		diffs    int
	)
	for _, s := range info.Samples {
		if !s.OK {
			fails++
			continue
		}
		ok++
		sum += s.Latency
		if ok == 1 || s.Latency < lo {
			lo = s.Latency
		}
		if ok == 1 || s.Latency > hi {
			hi = s.Latency
		}
		if havePrev {
			d := float64(s.Latency-prev) / float64(time.Millisecond)
			if d < 0 {
				d = -d
			}
			diffSum += d
			diffs++
		}
		prev = s.Latency
		havePrev = true
	}

	info.LossPct = float64(fails) / float64(len(info.Samples)) * 100
	if ok == 0 {
		return
	}
	info.AvgLatency = sum / time.Duration(ok)
	info.MinLatency = lo
	info.MaxLatency = hi
	if diffs > 0 {
		info.JitterMs = diffSum / float64(diffs)
	}
}

// sortPeers orders the list the way the GUI renders it: linked nodes first,
// then online before offline, then by display name. The final tiebreak on ID
// keeps the order stable across refreshes.
func sortPeers(peers []PeerInfo) {
	sort.Slice(peers, func(i, j int) bool {
		a, b := peers[i], peers[j]
		if a.Linked != b.Linked {
			return a.Linked
		}
		if a.Online != b.Online {
			return a.Online
		}
		if an, bn := strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName); an != bn {
			return an < bn
		}
		return a.ID < b.ID
	})
}

// peerKey is the stable identity used to key history. It falls back to the DNS
// name and then the first address for nodes without a stable ID.
func peerKey(ps *ipnstate.PeerStatus) string {
	if id := string(ps.ID); id != "" {
		return id
	}
	if dns := strings.TrimSuffix(ps.DNSName, "."); dns != "" {
		return dns
	}
	if len(ps.TailscaleIPs) > 0 {
		return ps.TailscaleIPs[0].String()
	}
	return ps.HostName
}

// displayName prefers the first label of the MagicDNS name, which is what the
// user typed in the config, then the reported hostname, then an address.
func displayName(ps *ipnstate.PeerStatus) string {
	if dns := strings.TrimSuffix(ps.DNSName, "."); dns != "" {
		if label, _, ok := strings.Cut(dns, "."); ok && label != "" {
			return label
		}
		return dns
	}
	if ps.HostName != "" {
		return ps.HostName
	}
	if len(ps.TailscaleIPs) > 0 {
		return ps.TailscaleIPs[0].String()
	}
	return string(ps.ID)
}

// pingAddr picks the address to probe, preferring IPv4 because that is what
// MagicDNS hands out for tailnet peers.
func pingAddr(ips []netip.Addr) netip.Addr {
	var v6 netip.Addr
	for _, ip := range ips {
		if ip.Is4() {
			return ip
		}
		if !v6.IsValid() {
			v6 = ip
		}
	}
	return v6
}

// mergeTags appends the tags missing from dst, keeping the result sorted and
// free of duplicates.
func mergeTags(dst, extra []string) []string {
	for _, t := range extra {
		i := sort.SearchStrings(dst, t)
		if i < len(dst) && dst[i] == t {
			continue
		}
		dst = append(dst, "")
		copy(dst[i+1:], dst[i:])
		dst[i] = t
	}
	return dst
}
