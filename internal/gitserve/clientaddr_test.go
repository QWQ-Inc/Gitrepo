package gitserve

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientAddr(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("127.0.0.1/32")}
	for _, c := range []struct {
		name, peer, xff, want string
		trusted               []netip.Prefix
	}{
		{"no proxies configured: header ignored", "203.0.113.7:4000", "198.51.100.1", "203.0.113.7", nil},
		{"untrusted peer: header ignored", "203.0.113.7:4000", "198.51.100.1", "203.0.113.7", trusted},
		{"trusted proxy: client from header", "127.0.0.1:4000", "198.51.100.1", "198.51.100.1", trusted},
		{"forged entries left of the client are ignored", "127.0.0.1:4000", "1.2.3.4, 198.51.100.1, 10.1.2.3", "198.51.100.1", trusted},
		{"trusted proxy without header", "127.0.0.1:4000", "", "127.0.0.1", trusted},
		{"malformed hop stops the walk", "127.0.0.1:4000", "198.51.100.1, junk", "127.0.0.1", trusted},
		{"all hops trusted: leftmost", "127.0.0.1:4000", "10.0.0.2, 10.0.0.1", "10.0.0.2", trusted},
		{"ipv6 peer", "[2001:db8::1]:4000", "198.51.100.1", "2001:db8::1", trusted},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.peer
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := ClientAddr(r, c.trusted); got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
