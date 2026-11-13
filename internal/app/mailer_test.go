package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/mail"
)

// noAWSProfile keeps the developer's own ~/.aws out of a test, so the region
// the test sees is only the one it sets.
func noAWSProfile(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "none")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	t.Setenv("AWS_CONFIG_FILE", empty)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", empty)
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
}

func sesConfig(mode string) *config.Config {
	return &config.Config{
		Server: config.Server{Mode: mode},
		Mail: config.Mail{
			Transport:  "ses",
			ConsoleURL: "https://app.cleargatefinance.xyz",
			From:       "Cleargate <no-reply@cleargatefinance.xyz>",
			SESRegion:  "eu-central-1",
		},
	}
}

// TestNewMailer_AcceptsSESOutsideDebug: SES is the transport a hosted
// environment runs. Building it reads no credential and calls nothing, so a
// missing task role shows on the first send, not at boot.
func TestNewMailer_AcceptsSESOutsideDebug(t *testing.T) {
	noAWSProfile(t)

	m, err := newMailer(context.Background(), sesConfig("release"), nil)

	require.NoError(t, err)
	assert.IsType(t, &mail.SESMailer{}, m)
}

func TestNewMailer_SESTakesTheRegionFromTheEnvironment(t *testing.T) {
	noAWSProfile(t)
	t.Setenv("AWS_REGION", "eu-central-1")
	cfg := sesConfig("release")
	cfg.Mail.SESRegion = ""

	_, err := newMailer(context.Background(), cfg, nil)

	assert.NoError(t, err)
}

func TestNewMailer_SESNeedsARegion(t *testing.T) {
	noAWSProfile(t)
	cfg := sesConfig("release")
	cfg.Mail.SESRegion = ""

	_, err := newMailer(context.Background(), cfg, nil)

	assert.ErrorIs(t, err, errNoMailTransport)
	assert.ErrorContains(t, err, "MAIL_SES_REGION")
}

func TestNewMailer_StillRefusesLogOutsideDebug(t *testing.T) {
	_, err := newMailer(context.Background(), &config.Config{Server: config.Server{Mode: "release"}, Mail: config.Mail{Transport: "log"}}, nil)
	assert.ErrorIs(t, err, errNoMailTransport)

	_, err = newMailer(context.Background(), &config.Config{Server: config.Server{Mode: "debug"}}, nil)
	assert.ErrorIs(t, err, errNoMailTransport, "no transport is chosen by default")

	_, err = newMailer(context.Background(), &config.Config{Server: config.Server{Mode: "debug"}, Mail: config.Mail{Transport: "smtp"}}, nil)
	assert.ErrorIs(t, err, errNoMailTransport, "an unknown transport refuses to start")
}
