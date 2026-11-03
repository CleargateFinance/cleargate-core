package server

import (
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
