package core

import (
	"net/netip"
	"time"

	"tailscale.com/ipn/ipnstate"
)

var Events chan interface{}

const (
	LinkInitFetchConfig           = 0
	LinkInitConnectingTailscale   = 1
	LinkInitControlPlaneConnected = 2
	LinkInitProgramSetup          = 3
	LinkInitReady
)

type LinkInitEvent struct {
	State int
}

type LinkErrorEvent struct {
	Error string
}

type LogEvent struct {
	Message string
}

type HostnameAssignedEvent struct {
	Hostname string
	IP       string
}

type LinkPeerConnectivityEvent struct {
	PingResult  []LinkPeerConnectivityStatus
	PerformedAt time.Time
}

type LinkPeerConnectivityStatus struct {
	Target netip.Addr
	Result *ipnstate.PingResult
}
