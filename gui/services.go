package gui

import (
	"net"
	"sort"
	"strings"

	"gioui.org/layout"
	"gioui.org/widget"

	"tslink/core"
)

// The services section answers "what did tslink open on this machine, and which
// server is behind it".
//
// It is built entirely from the parsed config plus the peer snapshot the app
// already holds — no multicast, no I/O on the render path. The previous LAN page
// listened for the same MOTD broadcasts tslink itself emits, which meant the
// list was assembled from packets: the same server appeared once per IP family,
// nothing deduplicated the two, and the rows were ordered by last-seen so a 1.5s
// broadcast cycle permuted them continuously. Deriving the list from config
// instead makes it exact and, because it is sorted on a stable key, still.

// serviceEntry is one local listener created by a connect rule.
type serviceEntry struct {
	Name  string // the rule's MOTD, or its config tag
	Tag   string // the [[connect.<tag>]] key
	Proto string
	Addr  string // the local address a client points at
	Port  int
	// Broadcast reports that this service is announced on the LAN, i.e. it
	// shows up in Minecraft's server list without being typed in.
	Broadcast bool
}

// serviceServer groups every local listener that targets one remote host.
type serviceServer struct {
	// Host is the dst_addr hostname, already MagicDNS-qualified by the
	// supervisor's NormalizeConnectRulesDstAddr pass.
	Host string
	// Peer is the tailnet node Host resolved to, when the peer monitor managed
	// to resolve it. Nil for destinations outside the tailnet, which are
	// legitimate config entries and must still render.
	Peer     *core.PeerInfo
	Services []serviceEntry
}

// Online reports the peer's reachability, defaulting to true when the
// destination is not a tailnet peer and we therefore have nothing to say.
func (s serviceServer) Online() bool { return s.Peer == nil || s.Peer.Online }

// Title is the friendliest name available for the target.
func (s serviceServer) Title() string {
	if s.Peer != nil && s.Peer.DisplayName != "" {
		return s.Peer.DisplayName
	}
	return s.Host
}

// serviceGroup collects the servers that resolve to one tailnet peer. A homelab
// subnet router that fronts several hosts — say tsdns-homelab carrying every
// *.homelab.ice destination — appears once, with its hosts nested beneath it,
// instead of as a run of sibling rows the user has to recognise as one machine.
type serviceGroup struct {
	// Peer is the node every server in the group resolved to, or nil when the
	// group is a single standalone destination outside the tailnet.
	Peer    *core.PeerInfo
	Servers []serviceServer
}

// Online mirrors serviceServer.Online: a group is down only when its peer is a
// known, offline tailnet node. Peerless (public) groups have nothing to report.
func (g serviceGroup) Online() bool { return g.Peer == nil || g.Peer.Online }

// Title is the group header: the peer's friendly name when it resolved, else the
// single host the group stands for.
func (g serviceGroup) Title() string {
	if g.Peer != nil && g.Peer.DisplayName != "" {
		return g.Peer.DisplayName
	}
	if len(g.Servers) > 0 {
		return g.Servers[0].Title()
	}
	return ""
}

// groupServices collapses the per-host servers into per-peer groups. Servers
// that resolved to the same tailnet node join one group; a server with no peer —
// an ordinary public destination — is a group of its own.
//
// The input is already sorted by buildServices (by title, then host), and
// servers behind one peer share a title, so they arrive adjacent and already
// host-ordered. First appearance fixes each group's position, so the section
// keeps the stable order buildServices established and never reshuffles between
// frames.
func groupServices(servers []serviceServer) []serviceGroup {
	groups := make([]serviceGroup, 0, len(servers))
	byPeer := make(map[string]int) // peer ID -> index into groups
	for _, srv := range servers {
		if srv.Peer == nil {
			groups = append(groups, serviceGroup{Servers: []serviceServer{srv}})
			continue
		}
		if idx, ok := byPeer[srv.Peer.ID]; ok {
			groups[idx].Servers = append(groups[idx].Servers, srv)
			continue
		}
		byPeer[srv.Peer.ID] = len(groups)
		groups = append(groups, serviceGroup{Peer: srv.Peer, Servers: []serviceServer{srv}})
	}
	return groups
}

// buildServices turns connect rules into the per-server view.
//
// Grouping is by destination host rather than by config tag: a server reached
// over both TCP and UDP is written as two tagged rules pointing at the same
// dst_addr, and the user thinks of that as one server with two services.
func buildServices(cfg *core.Config, snap core.PeerSnapshot) []serviceServer {
	if cfg == nil {
		return nil
	}

	// tag -> peer, via the links the monitor already resolved.
	byTag := make(map[string]*core.PeerInfo)
	for i := range snap.Peers {
		pr := &snap.Peers[i]
		for _, tag := range pr.LinkTags {
			byTag[tag] = pr
		}
	}

	grouped := make(map[string]*serviceServer)
	for tag, rules := range cfg.Connect {
		for _, rule := range rules {
			host := rule.DstAddr
			if h, _, err := net.SplitHostPort(rule.DstAddr); err == nil {
				host = h
			}
			g, ok := grouped[host]
			if !ok {
				g = &serviceServer{Host: host, Peer: byTag[tag]}
				grouped[host] = g
			} else if g.Peer == nil {
				g.Peer = byTag[tag]
			}
			g.Services = append(g.Services, serviceEntry{
				Name:      rule.LANMotdOr(tag),
				Tag:       tag,
				Proto:     rule.Protocol,
				Addr:      net.JoinHostPort(rule.BindIP(), itoa(rule.LocalPort)),
				Port:      rule.LocalPort,
				Broadcast: rule.LANEnabled(),
			})
		}
	}

	out := make([]serviceServer, 0, len(grouped))
	for _, g := range grouped {
		// Stable within a server: port, then protocol for the tcp/udp pair that
		// shares one.
		sort.SliceStable(g.Services, func(i, j int) bool {
			if g.Services[i].Port != g.Services[j].Port {
				return g.Services[i].Port < g.Services[j].Port
			}
			return g.Services[i].Proto < g.Services[j].Proto
		})
		out = append(out, *g)
	}
	// Ordered by what is actually on screen, so the list reads alphabetically
	// rather than by a hostname the user may never see. Host breaks ties and
	// keeps the order total — map iteration is randomised, so without a full
	// ordering the whole section would reshuffle every frame.
	sort.SliceStable(out, func(i, j int) bool {
		if ti, tj := out[i].Title(), out[j].Title(); ti != tj {
			return ti < tj
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// copyBtn lazily allocates a clickable per address.
func (p *overviewPage) copyBtn(addr string) *widget.Clickable {
	if p.svcCopy == nil {
		p.svcCopy = make(map[string]*widget.Clickable)
	}
	b, ok := p.svcCopy[addr]
	if !ok {
		b = new(widget.Clickable)
		p.svcCopy[addr] = b
	}
	return b
}

func (p *overviewPage) servicesCard(a *App, gtx C, servers []serviceServer) D {
	th := a.th
	card := th.Card()
	card.Title = th.T(KSvcTitle)
	card.Subtitle = th.T(KSvcSubtitle)

	groups := groupServices(servers)
	return card.Layout(th, gtx, func(gtx C) D {
		if len(groups) == 0 {
			return th.EmptyState(gtx, IconServer, th.T(KSvcEmpty), "")
		}
		children := make([]layout.FlexChild, 0, len(groups)*2)
		for i, g := range groups {
			if i > 0 {
				children = append(children, layout.Rigid(th.Divider))
			}
			children = append(children, layout.Rigid(func(gtx C) D {
				return p.serviceGroupRow(a, gtx, g)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

// serviceGroupRow renders one peer group. A group standing for a single host —
// whether a resolved peer or a bare public destination — keeps the flat
// one-server layout, so the common case looks exactly as it did before. A peer
// that fronts several hosts gets a header of its own with each host nested
// beneath it.
func (p *overviewPage) serviceGroupRow(a *App, gtx C, g serviceGroup) D {
	if g.Peer == nil || len(g.Servers) == 1 {
		return p.serverGroup(a, gtx, g.Servers[0])
	}
	return p.peerGroup(a, gtx, g)
}

// peerGroup renders a tailnet node and every host reached through it: one status
// dot and name for the node, then each destination host as a nested block.
func (p *overviewPage) peerGroup(a *App, gtx C, g serviceGroup) D {
	th := a.th
	level := LevelOK
	if !g.Online() {
		level = LevelFail
	}

	return layout.Inset{Top: SpaceSM, Bottom: SpaceSM}.Layout(gtx, func(gtx C) D {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return th.StatusDot(gtx, level, false)
					}),
					HGap(SpaceMD),
					layout.Flexed(1, func(gtx C) D {
						return OneLine(th.Body(g.Title())).Layout(gtx)
					}),
				)
			}),
		}
		for _, srv := range g.Servers {
			children = append(children, layout.Rigid(func(gtx C) D {
				return p.hostBlock(a, gtx, srv)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

// hostBlock renders one destination host nested under its peer group: the host
// name, then the services pointing at it. The peer's status and name already sit
// in the group header, so only the host and its ports repeat here.
func (p *overviewPage) hostBlock(a *App, gtx C, srv serviceServer) D {
	th := a.th
	return layout.Inset{Top: SpaceXS, Left: SpaceLG}.Layout(gtx, func(gtx C) D {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return IconServer(gtx, gtx.Dp(13), th.P.TextDim)
					}),
					HGap(SpaceSM),
					layout.Flexed(1, func(gtx C) D {
						return OneLine(th.MonoLabel(SizeCaption, th.P.TextSec, srv.Host)).Layout(gtx)
					}),
				)
			}),
		}
		for _, svc := range srv.Services {
			children = append(children, layout.Rigid(func(gtx C) D {
				return p.serviceRow(a, gtx, svc)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

// serverGroup renders one target host and the services pointing at it.
func (p *overviewPage) serverGroup(a *App, gtx C, srv serviceServer) D {
	th := a.th
	level := LevelOK
	if !srv.Online() {
		level = LevelFail
	}

	return layout.Inset{Top: SpaceSM, Bottom: SpaceSM}.Layout(gtx, func(gtx C) D {
		children := []layout.FlexChild{
			layout.Rigid(func(gtx C) D {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return th.StatusDot(gtx, level, false)
					}),
					HGap(SpaceMD),
					layout.Flexed(1, func(gtx C) D {
						return OneLine(th.Body(srv.Title())).Layout(gtx)
					}),
					layout.Rigid(func(gtx C) D {
						// Only worth showing when it differs from the title,
						// i.e. when the peer resolved to a nicer name.
						if srv.Peer == nil || srv.Title() == srv.Host {
							return D{}
						}
						return OneLine(th.MonoLabel(SizeCaption, th.P.TextDim, srv.Host)).Layout(gtx)
					}),
				)
			}),
		}
		for _, svc := range srv.Services {
			children = append(children, layout.Rigid(func(gtx C) D {
				return p.serviceRow(a, gtx, svc)
			}))
		}
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
	})
}

// serviceRow is the name-over-address entry: the address is the thing a user
// actually needs to type somewhere else, so it gets a monospace line of its own
// rather than being folded into the label.
func (p *overviewPage) serviceRow(a *App, gtx C, svc serviceEntry) D {
	th := a.th
	btn := p.copyBtn(svc.Addr)
	if btn.Clicked(gtx) {
		a.copyToClipboard(gtx, svc.Addr, "")
	}

	return layout.Inset{Top: 4, Bottom: 4, Left: SpaceLG}.Layout(gtx, func(gtx C) D {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx C) D {
				return IconLink(gtx, gtx.Dp(14), th.P.TextDim)
			}),
			HGap(SpaceMD),
			layout.Flexed(1, func(gtx C) D {
				return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
					layout.Rigid(func(gtx C) D {
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Rigid(OneLine(th.Body(svc.Name)).Layout),
							layout.Rigid(func(gtx C) D {
								if !svc.Broadcast {
									return D{}
								}
								return layout.Inset{Left: SpaceSM}.Layout(gtx, func(gtx C) D {
									return th.Chip(gtx, ChipStyle{
										Text:  th.T(KSvcBroadcast),
										Level: LevelInfo,
									})
								})
							}),
						)
					}),
					layout.Rigid(func(gtx C) D {
						return OneLine(th.MonoLabel(SizeCaption, th.P.TextSec, svc.Addr)).Layout(gtx)
					}),
				)
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				if svc.Proto == "" {
					return D{}
				}
				return th.Chip(gtx, ChipStyle{Text: strings.ToUpper(svc.Proto)})
			}),
			HGap(SpaceSM),
			layout.Rigid(func(gtx C) D {
				return th.IconButton(gtx, btn, IconCopy, LevelNeutral)
			}),
		)
	})
}
