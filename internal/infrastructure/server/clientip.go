package server

import (
	"net/netip"
	"strings"

	"github.com/gin-gonic/gin"
)

// ClientAddressFrom takes the client's address from the named header, set by
// the edge in front of the load balancer, and makes it the request's remote
// address. Everything that asks c.ClientIP afterwards (the public rate limit,
// the login throttle, the audit trail, the request log) then sees the person
// and not the edge, without knowing a header was involved.
//
// The header must be one the edge sets and a client cannot: behind CloudFront,
// CloudFront-Viewer-Address, with the load balancer accepting traffic from
// CloudFront alone (Task 13.6). A value that does not parse leaves the remote
// address as it was, the peer that connected, so the limiter always has a
// caller to count and never sees an empty address.
//
// Gin's own TrustedPlatform would return the header raw. CloudFront's value is
// ip:port, a new string for every connection, which would hand each connection
// a fresh rate-limit bucket.
func ClientAddressFrom(header string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if addr, ok := parseClientAddress(c.GetHeader(header)); ok {
			c.Request.RemoteAddr = addr.String()
		}
		c.Next()
	}
}

// parseClientAddress reads "ip:port", "[ipv6]:port" or a bare address. An
// IPv4 address mapped into IPv6 is unmapped, so one client has one key
// whichever form the edge chose.
func parseClientAddress(v string) (netip.AddrPort, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return netip.AddrPort{}, false
	}
	if ap, err := netip.ParseAddrPort(v); err == nil {
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), true
	}
	if a, err := netip.ParseAddr(v); err == nil {
		return netip.AddrPortFrom(a.Unmap(), 0), true
	}
	return netip.AddrPort{}, false
}
