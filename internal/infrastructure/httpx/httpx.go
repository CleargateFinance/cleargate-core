// Package httpx owns the Gin engine, the middleware chain and the single
// mapping from apperr.Kind to HTTP status. Modules receive a *gin.RouterGroup
// and never construct an engine themselves.
package httpx

// TODO(scaffold): NewEngine(cfg), error mapper, request-id, panic recovery.
