// Package mail delivers transactional email.
//
// It holds no business rules. Callers hand it an address and the single value
// the message is built around, and each implementation decides how that becomes
// a message and how it reaches the recipient: SESMailer sends it, and
// LoggingMailer writes its link to the log in development.
package mail

import (
	"context"
	"log/slog"
)

// LoggingMailer writes the message it would have sent to the log.
//
// It prints working credentials in clear text, so it is a development aid and
// nothing else. Nothing selects it by default: the composition root has to ask
// for it by name, which is what stops it reaching an environment where those
// log lines would be a live account-takeover vector.
type LoggingMailer struct {
	log   *slog.Logger
	links Links
}

// NewLogging builds a development mailer that logs links instead of sending
// them.
func NewLogging(log *slog.Logger, consoleURL string) *LoggingMailer {
	if log == nil {
		log = slog.Default()
	}
	return &LoggingMailer{log: log, links: NewLinks(consoleURL)}
}

// SendEmailVerification delivers the link that confirms an address.
func (m *LoggingMailer) SendEmailVerification(_ context.Context, to, token string) error {
	m.log.Warn("email verification not sent, logging the link instead",
		slog.String("to", to),
		slog.String("link", m.links.EmailVerification(token)),
		slog.String("mailer", "logging"),
	)
	return nil
}

// SendPasswordReset delivers the link that sets a new password.
func (m *LoggingMailer) SendPasswordReset(_ context.Context, to, token string) error {
	m.log.Warn("password reset not sent, logging the link instead",
		slog.String("to", to),
		slog.String("link", m.links.PasswordReset(token)),
		slog.String("mailer", "logging"),
	)
	return nil
}

// SendInvitation delivers the link that joins an account.
func (m *LoggingMailer) SendInvitation(_ context.Context, to, token, accountName string) error {
	m.log.Warn("invitation not sent, logging the link instead",
		slog.String("to", to),
		slog.String("account", accountName),
		slog.String("link", m.links.Invitation(token)),
		slog.String("mailer", "logging"),
	)
	return nil
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
