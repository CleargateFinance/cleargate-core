// Package mail delivers transactional email.
//
// It holds no templates and no business rules. Callers hand it an address and
// the single value the message is built around, and each implementation
// decides how that becomes a message and how it reaches the recipient.
package mail

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// LoggingMailer writes the message it would have sent to the log.
//
// It prints working credentials in clear text, so it is a development aid and
// nothing else. Nothing selects it by default: the composition root has to ask
// for it by name, which is what stops it reaching an environment where those
// log lines would be a live account-takeover vector.
type LoggingMailer struct {
	log *slog.Logger

	// consoleURL is where the links point, for example
	// https://console.cleargate.finance.
	consoleURL string
}

// NewLogging builds a development mailer that logs links instead of sending
// them.
func NewLogging(log *slog.Logger, consoleURL string) *LoggingMailer {
	if log == nil {
		log = slog.Default()
	}
	return &LoggingMailer{
		log:        log,
		consoleURL: strings.TrimSuffix(consoleURL, "/"),
	}
}

// SendEmailVerification delivers the link that confirms an address.
func (m *LoggingMailer) SendEmailVerification(_ context.Context, to, token string) error {
	m.log.Warn("email verification not sent, logging the link instead",
		slog.String("to", to),
		slog.String("link", m.link("/verify-email", token)),
		slog.String("mailer", "logging"),
	)
	return nil
}

// SendPasswordReset delivers the link that sets a new password.
func (m *LoggingMailer) SendPasswordReset(_ context.Context, to, token string) error {
	m.log.Warn("password reset not sent, logging the link instead",
		slog.String("to", to),
		slog.String("link", m.link("/reset-password", token)),
		slog.String("mailer", "logging"),
	)
	return nil
}

// SendInvitation delivers the link that joins an account.
func (m *LoggingMailer) SendInvitation(_ context.Context, to, token, accountName string) error {
	m.log.Warn("invitation not sent, logging the link instead",
		slog.String("to", to),
		slog.String("account", accountName),
		slog.String("link", m.link("/accept-invite", token)),
		slog.String("mailer", "logging"),
	)
	return nil
}

// link builds the URL a recipient would click.
//
// The token goes in the query string escaped, because it is base64url and a
// caller should never have to reason about whether that is safe to interpolate.
func (m *LoggingMailer) link(path, token string) string {
	return fmt.Sprintf("%s%s?token=%s", m.consoleURL, path, url.QueryEscape(token))
}

// DiscardMailer accepts every message and does nothing.
//
// Tests that are not asserting on delivery use it so their output stays
// readable.
type DiscardMailer struct{}

// SendEmailVerification does nothing and reports success.
func (DiscardMailer) SendEmailVerification(context.Context, string, string) error { return nil }

// SendPasswordReset does nothing and reports success.
func (DiscardMailer) SendPasswordReset(context.Context, string, string) error { return nil }

// SendInvitation does nothing and reports success.
func (DiscardMailer) SendInvitation(context.Context, string, string, string) error { return nil }
