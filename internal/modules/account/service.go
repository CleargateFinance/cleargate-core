package account

import (
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
)

// Default session policy.
//
// The lifetime is how long a console session is worth before a person signs in
// again. The cache window is unrelated and much shorter: it bounds how long a
// revoked session can survive when the delete that revocation performs does
// not happen.
const (
	DefaultSessionLifetime = 12 * time.Hour
	DefaultCacheWindow     = time.Minute

	// DefaultStepUpWindow is how recently a second factor must have been
	// satisfied for an action that creates authority or moves money.
	DefaultStepUpWindow = 15 * time.Minute

	// DefaultEmailTokenLifetime is how long a verification link stays usable.
	// Generous, because the message may sit unread in an inbox overnight.
	DefaultEmailTokenLifetime = 24 * time.Hour

	// DefaultResetTokenLifetime is how long a password reset link stays
	// usable. Much shorter, because it grants control of an account and a link
	// sitting in a mailbox is a liability for as long as it works.
	DefaultResetTokenLifetime = time.Hour
)

// DefaultMFAIssuer is the label shown in an authenticator app.
const DefaultMFAIssuer = "Cleargate"

// Service implements this module's use cases. It depends on the interfaces in
// ports.go, never on the storage or transport implementations directly.
type Service struct {
	repo     Repository
	hasher   PasswordHasher
	cache    SessionCache
	mailer   Mailer
	sealer   SecretSealer
	throttle Throttle
	clock    Clock
	log      *slog.Logger

	sessionLifetime    time.Duration
	cacheWindow        time.Duration
	stepUpWindow       time.Duration
	emailTokenLifetime time.Duration
	resetTokenLifetime time.Duration
	invitationLifetime time.Duration

	// mfaIssuer is the label an authenticator app shows beside the code, so a
	// person with several accounts can tell which one it belongs to.
	mfaIssuer string

	// consentVersions maps a document to the version currently shown at
	// sign-up, so what a person agreed to is recorded rather than assumed.
	consentVersions map[string]string
}

// Options configures a Service. Zero values select the defaults above, so a
// caller cannot accidentally configure a session that never expires or a cache
// entry that outlives its own revocation.
type Options struct {
	SessionLifetime    time.Duration
	CacheWindow        time.Duration
	StepUpWindow       time.Duration
	EmailTokenLifetime time.Duration
	ResetTokenLifetime time.Duration
	InvitationLifetime time.Duration

	// ConsentVersions carries the document versions in force, keyed by
	// DocumentTermsOfService and DocumentPrivacyPolicy. A document missing
	// from the map records no consent for it.
	ConsentVersions map[string]string

	// MFAIssuer labels the entry in a person's authenticator app. Empty
	// selects DefaultMFAIssuer.
	MFAIssuer string
}

// systemClock is the default time source, used when none is injected.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// NewService builds a Service. A nil clock selects the real one and a nil
// logger the default, so tests pass only what they care about.
func NewService(
	repo Repository,
	hasher PasswordHasher,
	cache SessionCache,
	mailer Mailer,
	sealer SecretSealer,
	throttle Throttle,
	clock Clock,
	log *slog.Logger,
	opts Options,
) *Service {
	if clock == nil {
		clock = systemClock{}
	}
	if log == nil {
		log = slog.Default()
	}
	if opts.SessionLifetime <= 0 {
		opts.SessionLifetime = DefaultSessionLifetime
	}
	if opts.CacheWindow <= 0 {
		opts.CacheWindow = DefaultCacheWindow
	}
	if opts.StepUpWindow <= 0 {
		opts.StepUpWindow = DefaultStepUpWindow
	}
	if opts.EmailTokenLifetime <= 0 {
		opts.EmailTokenLifetime = DefaultEmailTokenLifetime
	}
	if opts.ResetTokenLifetime <= 0 {
		opts.ResetTokenLifetime = DefaultResetTokenLifetime
	}
	if opts.InvitationLifetime <= 0 {
		opts.InvitationLifetime = DefaultInvitationLifetime
	}
	if opts.ConsentVersions == nil {
		opts.ConsentVersions = map[string]string{}
	}
	if opts.MFAIssuer == "" {
		opts.MFAIssuer = DefaultMFAIssuer
	}

	return &Service{
		repo:               repo,
		hasher:             hasher,
		cache:              cache,
		mailer:             mailer,
		sealer:             sealer,
		throttle:           throttle,
		clock:              clock,
		log:                log,
		sessionLifetime:    opts.SessionLifetime,
		cacheWindow:        opts.CacheWindow,
		stepUpWindow:       opts.StepUpWindow,
		emailTokenLifetime: opts.EmailTokenLifetime,
		resetTokenLifetime: opts.ResetTokenLifetime,
		invitationLifetime: opts.InvitationLifetime,
		consentVersions:    opts.ConsentVersions,
		mfaIssuer:          opts.MFAIssuer,
	}
}

// StepUpWindow reports how recently a second factor must have been satisfied
// for a sensitive action to proceed.
func (s *Service) StepUpWindow() time.Duration { return s.stepUpWindow }

// errInvalid builds a caller-facing validation error.
func errInvalid(message string) error {
	return apperr.Invalid("account: " + message)
}
