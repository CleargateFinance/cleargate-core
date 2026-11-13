package mail

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
)

// SESOptions configures SESMailer.
type SESOptions struct {
	// From is the sender, as an address or "Name <address>". Its domain must
	// be a verified SES identity in the client's region.
	From string

	// ConfigurationSet, if set, names the SES configuration set that publishes
	// bounces and complaints (Task 13.7). Empty sends without one.
	ConfigurationSet string

	// ConsoleURL is where the links point (CONSOLE_URL).
	ConsoleURL string
}

// sesSender is the one SES call the mailer makes, so tests can point a real
// client at a fake endpoint.
type sesSender interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, optFns ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SESMailer delivers transactional email through Amazon SES v2.
//
// It takes its AWS credentials from the client it is given, which in
// production comes from the default chain: on ECS, the task role. No key is
// ever in the environment. Messages carry no tracking pixel and no rewritten
// link, because a credential email whose links go through a redirector trains
// people to click redirectors.
type SESMailer struct {
	client    sesSender
	from      string
	configSet string
	links     Links
}

// NewSES builds the mailer. It refuses a sender that is not exactly one
// address, which SES would otherwise reject only on the first send.
func NewSES(client sesSender, opts SESOptions) (*SESMailer, error) {
	if client == nil {
		return nil, errors.New("mail: ses: no client")
	}
	if _, err := mail.ParseAddress(opts.From); err != nil {
		return nil, fmt.Errorf("mail: ses: MAIL_FROM %q is not one address: %w", opts.From, err)
	}
	return &SESMailer{
		client:    client,
		from:      opts.From,
		configSet: opts.ConfigurationSet,
		links:     NewLinks(opts.ConsoleURL),
	}, nil
}

// SendEmailVerification delivers the link that confirms an address.
func (m *SESMailer) SendEmailVerification(ctx context.Context, to, token string) error {
	return m.send(ctx, kindEmailVerification, to, messageData{Link: m.links.EmailVerification(token)})
}

// SendPasswordReset delivers the link that sets a new password.
func (m *SESMailer) SendPasswordReset(ctx context.Context, to, token string) error {
	return m.send(ctx, kindPasswordReset, to, messageData{Link: m.links.PasswordReset(token)})
}

// SendInvitation delivers the link that joins an account, naming the account
// so the recipient can tell an expected invitation from a phish.
func (m *SESMailer) SendInvitation(ctx context.Context, to, token, accountName string) error {
	return m.send(ctx, kindInvitation, to, messageData{Link: m.links.Invitation(token), AccountName: accountName})
}

// send renders and sends one message.
//
// Its error names the message and SES's error code and nothing else. Callers
// log it, and the link inside the message is a working credential, so neither
// the link, the token nor the recipient's address may appear in it.
func (m *SESMailer) send(ctx context.Context, k kind, to string, d messageData) error {
	msg, err := k.render(d)
	if err != nil {
		return err
	}

	in := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(m.from),
		Destination:      &types.Destination{ToAddresses: []string{to}},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(msg.Subject), Charset: aws.String("UTF-8")},
			Body: &types.Body{
				Text: &types.Content{Data: aws.String(msg.Text), Charset: aws.String("UTF-8")},
				Html: &types.Content{Data: aws.String(msg.HTML), Charset: aws.String("UTF-8")},
			},
		}},
	}
	if m.configSet != "" {
		in.ConfigurationSetName = aws.String(m.configSet)
	}

	if _, err := m.client.SendEmail(ctx, in); err != nil {
		return fmt.Errorf("mail: ses: send %s: %s", k.name, describe(err, to))
	}
	return nil
}

// describe reduces an SDK error to something safe to log. An API error keeps
// its code and message, with the recipient's address redacted, since SES's
// rejection names it. Any other error is the SDK's transport error, which names
// the endpoint and never the message body, so it is kept whole.
func describe(err error, to string) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		msg := api.ErrorMessage()
		if to != "" {
			msg = strings.ReplaceAll(msg, to, "<recipient>")
		}
		return api.ErrorCode() + ": " + msg
	}
	return err.Error()
}
