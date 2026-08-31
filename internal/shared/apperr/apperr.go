// Package apperr defines the error taxonomy shared by every module.
//
// Modules return semantic errors (NotFound, Conflict, Forbidden...). Exactly
// one place — platform/httpx — maps them to HTTP status codes, so no module
// needs to import net/http to report a failure.
package apperr

type Kind int

const (
	KindInternal Kind = iota
	KindNotFound
	KindInvalid
	KindConflict
	KindForbidden
	KindUnauthorized
	KindRateLimited
)
