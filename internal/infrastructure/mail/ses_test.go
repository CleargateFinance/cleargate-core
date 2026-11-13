package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentEmail is the part of SES v2's SendEmail request the tests read. It is
// decoded from the JSON the real SDK puts on the wire, so a field the SDK
// serialises differently fails here rather than in production.
type sentEmail struct {
	FromEmailAddress string
	Destination      struct{ ToAddresses []string }
	Content          struct {
		Simple struct {
			Subject struct{ Data string }
			Body    struct {
				Text struct{ Data string }
				HTML struct{ Data string } `json:"Html"`
			}
		}
	}
	ConfigurationSetName string
}

// fakeSES stands in for the SES v2 endpoint. It records every SendEmail and
// answers with status, which is 200 unless a test sets it.
type fakeSES struct {
	mu     sync.Mutex
	sent   []sentEmail
	status int
	srv    *httptest.Server
}

func newFakeSES(t *testing.T) *fakeSES {
	t.Helper()
	f := &fakeSES{status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST /v2/email/outbound-emails", r.Method+" "+r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("Authorization"), "the request is signed with the default credentials")

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var e sentEmail
		require.NoError(t, json.Unmarshal(body, &e))

		f.mu.Lock()
		f.sent = append(f.sent, e)
		status := f.status
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.Header().Set("X-Amzn-Errortype", "MessageRejected")
			w.WriteHeader(status)
			// SES's real rejection names the address that failed the check.
			_, _ = w.Write([]byte(`{"message":"Email address is not verified. The following identities failed the check in region EU-CENTRAL-1: ada@example.com"}`))
			return
		}
		_, _ = w.Write([]byte(`{"MessageId":"0100018c-test"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSES) only(t *testing.T) sentEmail {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.sent, 1)
	return f.sent[0]
}

// mailer builds an SESMailer against the fake, with static test credentials
// and a single attempt, so an error case does not sit through retries.
func (f *fakeSES) mailer(t *testing.T, opts SESOptions) *SESMailer {
	t.Helper()
	client := sesv2.New(sesv2.Options{
		Region:           "eu-central-1",
		BaseEndpoint:     aws.String(f.srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AKIDTEST", "secret", ""),
		HTTPClient:       f.srv.Client(),
		RetryMaxAttempts: 1,
	})
	if opts.From == "" {
		opts.From = "Cleargate <no-reply@cleargatefinance.xyz>"
	}
	if opts.ConsoleURL == "" {
		opts.ConsoleURL = "https://app.cleargatefinance.xyz/"
	}
	m, err := NewSES(client, opts)
	require.NoError(t, err)
	return m
}

const testToken = "tok_Abc-123_xyz"

func TestSESMailer_SendsVerificationLink(t *testing.T) {
	f := newFakeSES(t)

	err := f.mailer(t, SESOptions{}).SendEmailVerification(context.Background(), "ada@example.com", testToken)

	require.NoError(t, err)
	e := f.only(t)
	assert.Equal(t, []string{"ada@example.com"}, e.Destination.ToAddresses)
	assert.Equal(t, "Cleargate <no-reply@cleargatefinance.xyz>", e.FromEmailAddress)
	assert.NotEmpty(t, e.Content.Simple.Subject.Data)

	link := "https://app.cleargatefinance.xyz/verify-email?token=" + testToken
	assert.Equal(t, 1, strings.Count(e.Content.Simple.Body.Text.Data, testToken), "the token appears once, in the link")
	assert.Contains(t, e.Content.Simple.Body.Text.Data, link)
	assert.Contains(t, e.Content.Simple.Body.HTML.Data, link)
}

func TestSESMailer_SendsPasswordResetAndInvitationLinks(t *testing.T) {
	f := newFakeSES(t)
	m := f.mailer(t, SESOptions{})

	require.NoError(t, m.SendPasswordReset(context.Background(), "ada@example.com", testToken))
	require.NoError(t, m.SendInvitation(context.Background(), "bo@example.com", testToken, "Acme Research"))

	require.Len(t, f.sent, 2)
	assert.Contains(t, f.sent[0].Content.Simple.Body.Text.Data, "https://app.cleargatefinance.xyz/reset-password?token="+testToken)
	assert.Contains(t, f.sent[1].Content.Simple.Body.Text.Data, "https://app.cleargatefinance.xyz/accept-invite?token="+testToken)
	assert.Contains(t, f.sent[1].Content.Simple.Body.Text.Data, "Acme Research",
		"an unexpected invitation names the account, or it reads like a phish")
}

// TestSESMailer_ErrorIsReturnedNotSwallowed: the account service logs the
// error text, so it must say what failed without carrying the token, which is
// a working credential.
func TestSESMailer_ErrorIsReturnedNotSwallowed(t *testing.T) {
	f := newFakeSES(t)
	f.status = http.StatusBadRequest

	err := f.mailer(t, SESOptions{}).SendEmailVerification(context.Background(), "ada@example.com", testToken)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "MessageRejected")
	assert.NotContains(t, err.Error(), testToken)
	assert.NotContains(t, err.Error(), "ada@example.com", "no address in a log line either")
}

func TestSESMailer_TransportErrorIsReturned(t *testing.T) {
	f := newFakeSES(t)
	m := f.mailer(t, SESOptions{})
	f.srv.Close()

	err := m.SendEmailVerification(context.Background(), "ada@example.com", testToken)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), testToken)
}

func TestSESMailer_UsesTheConfigurationSet(t *testing.T) {
	f := newFakeSES(t)

	require.NoError(t, f.mailer(t, SESOptions{ConfigurationSet: "cleargate-staging"}).
		SendPasswordReset(context.Background(), "ada@example.com", testToken))

	assert.Equal(t, "cleargate-staging", f.only(t).ConfigurationSetName)
}

// TestSESMailer_EscapesTheAccountName: an account's name is chosen by whoever
// created it, so it reaches the HTML part escaped and never the subject.
func TestSESMailer_EscapesTheAccountName(t *testing.T) {
	f := newFakeSES(t)
	name := `<a href="https://evil.example">Acme</a>`

	require.NoError(t, f.mailer(t, SESOptions{}).SendInvitation(context.Background(), "bo@example.com", testToken, name))

	e := f.only(t)
	assert.NotContains(t, e.Content.Simple.Body.HTML.Data, `<a href="https://evil.example">`)
	assert.Contains(t, e.Content.Simple.Body.HTML.Data, "&lt;a href=")
	assert.NotContains(t, e.Content.Simple.Subject.Data, "evil")
}

// TestSESMailer_NoTracking: a rewritten or tracked link in a credential email
// trains people to click redirectors, so the only link is the console's own.
func TestSESMailer_NoTracking(t *testing.T) {
	f := newFakeSES(t)

	require.NoError(t, f.mailer(t, SESOptions{}).SendEmailVerification(context.Background(), "ada@example.com", testToken))

	html := f.only(t).Content.Simple.Body.HTML.Data
	assert.NotContains(t, html, "<img", "no tracking pixel")
	assert.Equal(t, 1, strings.Count(html, "href="), "one link, to the console")
}

func TestNewSES_RefusesABadSender(t *testing.T) {
	client := sesv2.New(sesv2.Options{Region: "eu-central-1"})

	for _, from := range []string{"", "not an address", "a@b.c, d@e.f"} {
		_, err := NewSES(client, SESOptions{From: from, ConsoleURL: "https://app.cleargatefinance.xyz"})
		assert.Error(t, err, from)
	}
}

// TestLinks_MatchAcrossTransports: the logging mailer and SES build their links
// in one place, so a development run shows exactly the URL a person would get.
func TestLinks_MatchAcrossTransports(t *testing.T) {
	l := NewLinks("https://app.cleargatefinance.xyz/")

	assert.Equal(t, "https://app.cleargatefinance.xyz/verify-email?token=a%2Bb%2Fc", l.EmailVerification("a+b/c"),
		"the token is query-escaped whatever it contains")
	assert.Equal(t, "https://app.cleargatefinance.xyz/reset-password?token=t", l.PasswordReset("t"))
	assert.Equal(t, "https://app.cleargatefinance.xyz/accept-invite?token=t", l.Invitation("t"))
}
