package netdiag

// This file holds the STUN probe target lists. The default list deliberately
// mixes mainland-China and international servers: when a proxy, a split tunnel
// or the GFW is in play the two groups disagree, and that disagreement is the
// diagnostic signal we are after. Probing only one side would hide it.

// DefaultSTUNServers returns the built-in probe list, covering both mainland
// China (RegionCN) and international (RegionIntl) targets.
//
// A fresh slice is returned on every call so callers may reorder or trim it
// without affecting anyone else.
func DefaultSTUNServers() []STUNServer {
	return []STUNServer{
		// Mainland China. These answer fast from inside the country and are the
		// baseline for "does UDP work at all on this line".
		{Host: "stun.miwifi.com:3478", Name: "小米", Region: RegionCN},
		{Host: "stun.chat.bilibili.com:3478", Name: "哔哩哔哩", Region: RegionCN},
		{Host: "stun.qq.com:3478", Name: "腾讯", Region: RegionCN},
		{Host: "stun.hitv.com:3478", Name: "芒果TV", Region: RegionCN},
		// Anycast: usually lands on an in-country PoP, so it is grouped with CN
		// even though the operator is not Chinese.
		{Host: "turn.cloudflare.com:3478", Name: "Cloudflare(任播)", Region: RegionCN},

		// International. Failures here while the CN group succeeds mean egress
		// to the wider internet is filtered rather than UDP being dead.
		{Host: "stun.l.google.com:19302", Name: "Google", Region: RegionIntl},
		{Host: "stun.cloudflare.com:3478", Name: "Cloudflare", Region: RegionIntl},
		{Host: "stun.nextcloud.com:3478", Name: "Nextcloud", Region: RegionIntl},
		{Host: "stun.voip.blackberry.com:3478", Name: "BlackBerry", Region: RegionIntl},
		{Host: "stun.sipnet.net:3478", Name: "SipNet", Region: RegionIntl},
		{Host: "stun.stunprotocol.org:3478", Name: "StunProtocol", Region: RegionIntl},
		{Host: "stun.voipgate.com:3478", Name: "VoIPGate", Region: RegionIntl},
	}
}

// RFC5780Servers returns targets known to implement RFC 5780 behaviour
// discovery, i.e. they advertise OTHER-ADDRESS and actually honour
// CHANGE-REQUEST by answering from a second IP and/or port.
//
// Only such servers can drive the filtering-behaviour test in [ClassifyNAT].
// Most large providers — Google and Cloudflare among them — answer plain
// binding requests perfectly well but silently ignore CHANGE-REQUEST and never
// send OTHER-ADDRESS, so a probe against them looks identical to a firewall
// dropping the reply. Classification therefore has to degrade gracefully: when
// no reachable server honours CHANGE-REQUEST, filtering behaviour stays
// [BehaviorUnknown] and the NAT type is reported as [NATUnknown] with an
// explanatory note rather than being guessed.
//
// This list is a floor, not a whitelist: [stunWithRFC5780] merges it into
// whatever server list the caller passed so a custom list of Google/Cloudflare
// style endpoints still has something to test against, and the filtering pass
// will happily use any *other* reachable target that turns out to advertise an
// OTHER-ADDRESS. It must include in-country options — the international
// entries below are unreachable or DNS-hijacked on a good many networks, and
// when they were the only candidates the filtering test could never run.
func RFC5780Servers() []STUNServer {
	return []STUNServer{
		// Mainland China. Verified to advertise OTHER-ADDRESS and to answer
		// both CHANGE-REQUEST variants from the advertised alternate.
		{Host: "stun.miwifi.com:3478", Name: "小米", Region: RegionCN},
		{Host: "stun.hitv.com:3478", Name: "芒果TV", Region: RegionCN},

		{Host: "stun.sipnet.net:3478", Name: "SipNet", Region: RegionIntl},
		{Host: "stun.voipgate.com:3478", Name: "VoIPGate", Region: RegionIntl},
		{Host: "stun.stunprotocol.org:3478", Name: "StunProtocol", Region: RegionIntl},
	}
}

// stunWithRFC5780 appends the RFC 5780 servers a caller's list does not already
// cover, so behaviour discovery always has candidates that can actually drive a
// CHANGE-REQUEST. They go at the end: the caller's own targets are tried first,
// and resolution happens in one parallel pass, so the extras cost nothing when
// the caller's list already works.
func stunWithRFC5780(servers []STUNServer) []STUNServer {
	have := make(map[string]bool, len(servers))
	for _, s := range servers {
		have[s.Host] = true
	}
	out := append([]STUNServer(nil), servers...)
	for _, s := range RFC5780Servers() {
		if !have[s.Host] {
			out = append(out, s)
			have[s.Host] = true
		}
	}
	return out
}
