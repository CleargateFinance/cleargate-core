package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientIP serves one request from peer, carrying forwarded as its
// X-Forwarded-For, and returns the address c.ClientIP settled on.
func clientIP(t *testing.T, opts Options, peer, forwarded string) string {
	t.Helper()
	engine, err := New(opts, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	var got string
	engine.GET("/", func(c *gin.Context) { got = c.ClientIP() })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer + ":40000"
	req.Header.Set("X-Forwarded-For", forwarded)
	engine.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestNew_BelievesForwardedForOnlyFromATrustedProxy(t *testing.T) {
	assert.Equal(t, "203.0.113.9", clientIP(t, Options{}, "203.0.113.9", "198.51.100.1"),
		"with no trusted proxy a client cannot name its own address")
	assert.Equal(t, "198.51.100.1", clientIP(t, Options{TrustedProxies: []string{"10.0.0.0/8"}}, "10.1.2.3", "198.51.100.1"),
		"the load balancer's header is believed")
	assert.Equal(t, "203.0.113.9", clientIP(t, Options{TrustedProxies: []string{"10.0.0.0/8"}}, "203.0.113.9", "198.51.100.1"),
		"but only from the load balancer")
}

func TestNew_RefusesAProxyThatIsNotAnAddress(t *testing.T) {
	_, err := New(Options{TrustedProxies: []string{"not-an-ip"}}, slog.New(slog.DiscardHandler))
	assert.Error(t, err)
}

// viewerIP serves one request from peer with the given headers, through an
// engine that takes the client's address from CloudFront-Viewer-Address, and
// returns the address c.ClientIP settled on.
func viewerIP(t *testing.T, peer string, headers map[string]string) string {
	t.Helper()
	engine, err := New(Options{ClientIPHeader: "CloudFront-Viewer-Address"}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	var got string
	engine.GET("/", func(c *gin.Context) { got = c.ClientIP() })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = peer + ":40000"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	engine.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

// TestClientIP_FromViewerAddressHeader: CloudFront sends ip:port, with IPv6 in
// brackets. The port is dropped, or every connection would be its own rate
// limit bucket and the limit would be no limit at all.
func TestClientIP_FromViewerAddressHeader(t *testing.T) {
	const alb = "10.0.1.20"
	cases := map[string]string{
		"198.51.100.7:46532":        "198.51.100.7",
		"[2001:db8::1]:443":         "2001:db8::1",
		"[::ffff:198.51.100.7]:443": "198.51.100.7",
		"198.51.100.7":              "198.51.100.7",
	}
	for header, want := range cases {
		assert.Equal(t, want, viewerIP(t, alb, map[string]string{"CloudFront-Viewer-Address": header}), header)
	}
}

// TestClientIP_IgnoresForwardedForWhenHeaderConfigured: X-Forwarded-For is
// whatever the client wrote plus what each hop appended, so with the viewer
// header in use it is not consulted at all.
func TestClientIP_IgnoresForwardedForWhenHeaderConfigured(t *testing.T) {
	got := viewerIP(t, "10.0.1.20", map[string]string{
		"CloudFront-Viewer-Address": "198.51.100.7:46532",
		"X-Forwarded-For":           "203.0.113.66",
	})
	assert.Equal(t, "198.51.100.7", got)

	got = viewerIP(t, "10.0.1.20", map[string]string{"X-Forwarded-For": "203.0.113.66"})
	assert.Equal(t, "10.0.1.20", got, "no viewer header: the peer, never the forwarded address")
}

// TestClientIP_MalformedHeaderFallsBackToPeer: never an empty address, which
// the public limiter would treat as a caller it cannot name and let through.
func TestClientIP_MalformedHeaderFallsBackToPeer(t *testing.T) {
	for _, header := range []string{"garbage", "198.51.100.7:notaport", "[2001:db8::1", "198.51.100.7, 203.0.113.66", " "} {
		assert.Equal(t, "10.0.1.20", viewerIP(t, "10.0.1.20", map[string]string{"CloudFront-Viewer-Address": header}), header)
	}
}

func TestNew_RefusesAHeaderAndTrustedProxiesTogether(t *testing.T) {
	_, err := New(Options{ClientIPHeader: "CloudFront-Viewer-Address", TrustedProxies: []string{"10.0.0.0/8"}}, slog.New(slog.DiscardHandler))
	assert.Error(t, err)
}

// TestNew_WarnsInDebugWhenTrustingAHeader: locally nothing sits in front of
// the API, so any client can set the header and choose its own address.
func TestNew_WarnsInDebugWhenTrustingAHeader(t *testing.T) {
	var buf bytes.Buffer
	_, err := New(Options{Mode: "debug", ClientIPHeader: "CloudFront-Viewer-Address"}, slog.New(slog.NewTextHandler(&buf, nil)))
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "CloudFront-Viewer-Address")
	assert.Contains(t, buf.String(), "level=WARN")
}
