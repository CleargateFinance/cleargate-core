package server

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/logger"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
)

// errorResponse is the single error shape the API ever returns.
//
// One shape everywhere means SDK clients write one error branch, not one per
// endpoint.
type errorResponse struct {
	// Error is the caller-safe message. Internal detail never reaches it.
	Error string `json:"error"`
	// Kind is the stable machine-readable category, safe for clients to
	// branch on. Unlike the message, it will not be reworded.
	Kind string `json:"kind"`
	// RequestID lets a caller quote one ID in a support request.
	RequestID string `json:"request_id,omitempty"`
}

// statusFor maps a semantic error kind to an HTTP status code.
//
// This function is the ONLY place in the codebase that makes this translation.
// That is what lets every module return apperr values without importing
// net/http, keeping business logic free of transport concerns.
func statusFor(kind apperr.Kind) int {
	switch kind {
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindInvalid:
		return http.StatusBadRequest
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindUnauthorized:
		return http.StatusUnauthorized
	case apperr.KindRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// ErrorMapper turns errors a handler recorded into an HTTP response.
//
// Handlers do not write error responses themselves. They call c.Error(err) and
// return, then this middleware translates. That keeps the mapping in one place
// instead of scattered across every handler, and it means a handler cannot
// accidentally return a 200 alongside an error.
//
// If a handler already wrote a response, nothing is overwritten, since the
// status is then already committed.
func ErrorMapper() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		if len(c.Errors) == 0 || c.Writer.Written() {
			return
		}

		// The last error is the most specific one, closest to the failure.
		err := c.Errors.Last().Err
		kind := apperr.KindOf(err)
		status := statusFor(kind)

		// A 5xx means we did something unexpected, so log the full error
		// including any wrapped cause. Client errors are the caller's problem
		// and are already visible in the request log.
		if status >= http.StatusInternalServerError {
			logger.FromContext(c.Request.Context()).Error("handler error",
				slog.String("error", err.Error()),
				slog.String("kind", kind.String()),
			)
		}

		c.JSON(status, errorResponse{
			// MessageOf returns the caller-safe message, or a generic string
			// for errors that are not apperr values, so internals never leak.
			Error:     apperr.MessageOf(err),
			Kind:      kind.String(),
			RequestID: c.GetString("request_id"),
		})
	}
}
