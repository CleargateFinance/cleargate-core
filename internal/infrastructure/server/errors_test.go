package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
)

func TestErrorMapper_SendsCodeAndRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ErrorMapper())
	r.GET("/", func(c *gin.Context) {
		err := apperr.RateLimited("slow down").WithCode("too_many_attempts").WithRetryAfter(1500 * time.Millisecond)
		_ = c.Error(fmt.Errorf("wrapped: %w", err))
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "2", w.Header().Get("Retry-After"), "rounded up to whole seconds")

	var body errorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "rate_limited", body.Kind)
	assert.Equal(t, "too_many_attempts", body.Code)
}

func TestErrorMapper_OmitsCodeWhenThereIsNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(ErrorMapper())
	r.GET("/", func(c *gin.Context) { _ = c.Error(apperr.NotFound("missing")) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Empty(t, w.Header().Get("Retry-After"))
	assert.NotContains(t, w.Body.String(), `"code"`)
}

func TestWithCode_LeavesSentinelUnchanged(t *testing.T) {
	sentinel := apperr.Conflict("taken")
	_ = sentinel.WithCode("x").WithRetryAfter(time.Second)

	assert.Empty(t, sentinel.Code)
	assert.Zero(t, sentinel.RetryAfter)
}
