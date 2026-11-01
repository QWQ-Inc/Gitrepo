package gitserve

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientAddr returns the address of the client behind r. X-Forwarded-For
// is used only when the direct peer is one of the trusted proxies, and then
// read from the right: the first address that is not itself a trusted proxy
// is the client (anything left of it may be forged).
func ClientAddr(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !isTrusted(peer.Unmap(), trusted) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	client := peer.Unmap()
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		a, err := netip.ParseAddr(h)
		if err != nil {
			break // malformed: stop at the last address we could verify
		}
		client = a.Unmap()
		if !isTrusted(client, trusted) {
			break
		}
	}
	return client.String()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
