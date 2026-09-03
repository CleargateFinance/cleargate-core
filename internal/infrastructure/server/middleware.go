package server

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/logger"
)

// RequestIDHeader is the header carrying the request's trace ID, both inbound
// and outbound.
const RequestIDHeader = "X-Request-ID"

// RequestID assigns every request a unique ID, or reuses the one the caller
// sent.
//
// Reusing an inbound ID is what makes a trace span more than one service. When
// the SDK sends its own ID, that same ID appears in our logs, so a customer
// reporting "request abc-123 failed" can be found directly.
//
// The ID goes three places: the response header so the caller can see it, the
// Gin context for handlers, and a request-scoped logger on the request context
// so any code deeper in the call chain logs it automatically.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}

		c.Set("request_id", id)
		c.Header(RequestIDHeader, id)

		// Attach a logger already tagged with the ID, then swap the request so
		// everything downstream sees the enriched context.
		log := logger.FromContext(c.Request.Context()).With(slog.String("request_id", id))
		c.Request = c.Request.WithContext(logger.WithContext(c.Request.Context(), log))

		c.Next()
	}
}

// RequestLogger logs one structured line per completed request.
//
// It runs the rest of the chain first, then logs, so the status code and
// duration it records are the final ones.
func RequestLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		attrs := []any{
			slog.String("method", c.Request.Method),
			slog.String("path", path),
			slog.Int("status", c.Writer.Status()),
			slog.Duration("latency", time.Since(start)),
			slog.String("request_id", c.GetString("request_id")),
		}

		// A 5xx is our fault and deserves error level, a 4xx is the caller's
		// and is only worth a warning, anything else is routine.
		switch {
		case c.Writer.Status() >= http.StatusInternalServerError:
			log.Error("request failed", attrs...)
		case c.Writer.Status() >= http.StatusBadRequest:
			log.Warn("request rejected", attrs...)
		default:
			log.Info("request", attrs...)
		}
	}
}

// Recovery turns a panic into a 500 instead of letting it kill the process.
//
// Without this, one nil dereference in a single handler would take down the
// server for every other in-flight request. The panic is logged with its
// request ID so it can still be investigated.
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic recovered",
					slog.Any("panic", r),
					slog.String("path", c.Request.URL.Path),
					slog.String("request_id", c.GetString("request_id")),
				)

				// AbortWithStatusJSON stops the remaining handlers and writes
				// the response. The client learns nothing about the panic,
				// only that something failed on our side.
				c.AbortWithStatusJSON(http.StatusInternalServerError, errorResponse{
					Error:     "internal error",
					Kind:      "internal",
					RequestID: c.GetString("request_id"),
				})
			}
		}()

		c.Next()
	}
}
