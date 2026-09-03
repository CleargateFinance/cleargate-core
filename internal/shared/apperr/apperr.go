// Package apperr defines the error taxonomy shared by every module.
//
// Modules return semantic errors (NotFound, Conflict, Forbidden...). Exactly
// one place — infrastructure/server — maps them to HTTP status codes, so no
// module needs to import net/http to report a failure.
package apperr

import (
	"errors"
	"fmt"
)

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

// String makes Kind readable in logs.
func (k Kind) String() string {
	switch k {
	case KindNotFound:
		return "not_found"
	case KindInvalid:
		return "invalid"
	case KindConflict:
		return "conflict"
	case KindForbidden:
		return "forbidden"
	case KindUnauthorized:
		return "unauthorized"
	case KindRateLimited:
		return "rate_limited"
	default:
		return "internal"
	}
}

// Error carries a Kind alongside a message, and optionally wraps the
// underlying cause.
//
// The message is safe to show a caller. Anything sensitive belongs in the
// wrapped error, which is logged but never returned over the wire.
type Error struct {
	Kind    Kind
	Message string
	cause   error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Unwrap exposes the wrapped cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

// New builds an Error of the given kind.
func New(kind Kind, message string) *Error {
	return &Error{Kind: kind, Message: message}
}

// Wrap builds an Error of the given kind that carries an underlying cause.
func Wrap(kind Kind, message string, cause error) *Error {
	return &Error{Kind: kind, Message: message, cause: cause}
}

// KindOf reports the Kind of err, walking the wrap chain.
//
// Anything that is not an *Error is treated as KindInternal, which is the safe
// default, since an unclassified error is by definition one we did not expect.
func KindOf(err error) Kind {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Kind
	}
	return KindInternal
}

// MessageOf returns the caller-safe message for err, or a generic string when
// err is not an *Error. Internal details never leak through this function.
func MessageOf(err error) string {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr.Message
	}
	return "internal error"
}

// Constructors for the common kinds, so call sites read as prose.

// NotFound reports that a requested resource does not exist.
func NotFound(message string) *Error { return New(KindNotFound, message) }

// Invalid reports that the caller's input failed validation.
func Invalid(message string) *Error { return New(KindInvalid, message) }

// Conflict reports that the request clashes with current state, for example a
// reused idempotency key carrying different parameters.
func Conflict(message string) *Error { return New(KindConflict, message) }

// Forbidden reports that the caller is known but not allowed to do this.
func Forbidden(message string) *Error { return New(KindForbidden, message) }

// Unauthorized reports that the caller is not authenticated.
func Unauthorized(message string) *Error { return New(KindUnauthorized, message) }

// RateLimited reports that the caller has exceeded its rate limit.
func RateLimited(message string) *Error { return New(KindRateLimited, message) }

// Internal reports an unexpected failure, wrapping the cause for the logs.
func Internal(message string, cause error) *Error { return Wrap(KindInternal, message, cause) }
