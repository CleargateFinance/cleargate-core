package mail

import (
	"fmt"
	"net/url"
	"strings"
)

// Links builds the console URLs a message carries. Every transport builds its
// links here, so a development run's logged link is exactly the URL a person
// would be sent, and the paths the console must serve are written down once.
type Links struct {
	// base is the console's address with no trailing slash, for example
	// https://app.cleargatefinance.xyz.
	base string
}

// NewLinks builds links rooted at consoleURL (CONSOLE_URL).
func NewLinks(consoleURL string) Links {
	return Links{base: strings.TrimSuffix(consoleURL, "/")}
}

// EmailVerification is the link that confirms an address.
func (l Links) EmailVerification(token string) string { return l.build("/verify-email", token) }

// PasswordReset is the link that sets a new password.
func (l Links) PasswordReset(token string) string { return l.build("/reset-password", token) }

// Invitation is the link that joins an account.
func (l Links) Invitation(token string) string { return l.build("/accept-invite", token) }

// build puts the token in the query string escaped, because it is base64url
// and a caller should never have to reason about whether that is safe to
// interpolate.
func (l Links) build(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", l.base, path, url.QueryEscape(token))
}
