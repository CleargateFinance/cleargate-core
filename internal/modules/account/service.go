package account

import (
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"

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

	// tierLimits is the funding ceiling per verification tier.
	tierLimits TierLimits

	// funding reports how much a principal has funded to date. Nil until the
	// funding flow is wired, which is why RequireFundingAllowed refuses rather
	// than assuming zero — a cap measured against an assumption is not a cap.
	funding FundingHistory
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

	// TierLimits overrides the funding ceilings. A zero value selects the
	// defaults, so a deployment that does not care cannot accidentally
	// configure an unverified account with no cap at all.
	TierLimits *TierLimits

	// FundingHistory supplies funded-to-date from the ledger. Required before
	// any funding path runs, and deliberately absent until one exists.
	FundingHistory FundingHistory
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

	limits, err := defaultTierLimits()
	if err != nil {
		// The defaults are compile-time constants, so this cannot fail unless
		// somebody edits them into something unparseable. Panicking at
		// construction is the right answer: a service with no funding ceiling
		// must never start.
		panic("account: default tier limits are invalid: " + err.Error())
	}
	if opts.TierLimits != nil {
		limits = *opts.TierLimits
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
		tierLimits:         limits,
		funding:            opts.FundingHistory,
	}
}

// defaultTierLimits parses the compile-time ceilings into exact amounts.
func defaultTierLimits() (TierLimits, error) {
	basic, err := currency.Parse(DefaultBasicFundingCap, currency.USD)
	if err != nil {
		return TierLimits{}, err
	}
	verified, err := currency.Parse(DefaultVerifiedFundingCap, currency.USD)
	if err != nil {
		return TierLimits{}, err
	}
	business, err := currency.Parse(DefaultBusinessFundingCap, currency.USD)
	if err != nil {
		return TierLimits{}, err
	}
	return TierLimits{Basic: basic, Verified: verified, Business: business}, nil
}

// StepUpWindow reports how recently a second factor must have been satisfied
// for a sensitive action to proceed.
func (s *Service) StepUpWindow() time.Duration { return s.stepUpWindow }

// errInvalid builds a caller-facing validation error.
func errInvalid(message string) error {
	return apperr.Invalid("account: " + message)
}
