// Package apperr defines the error taxonomy shared by every module.
//
// Modules return semantic errors (NotFound, Conflict, Forbidden...). Exactly
// one place — platform/httpx — maps them to HTTP status codes, so no module
// needs to import net/http to report a failure.
package apperr

// Kind classifies an error into one of a small, fixed set of semantic
// categories, independent of any transport (HTTP, gRPC, ...).
type Kind int

// The fixed set of error kinds every module may return.
const (
	KindInternal Kind = iota
	KindNotFound
	KindInvalid
	KindConflict
	KindForbidden
	KindUnauthorized
	KindRateLimited
)
