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
)

// Service implements this module's use cases. It depends on the interfaces in
// ports.go, never on the storage or transport implementations directly.
type Service struct {
	repo   Repository
	hasher PasswordHasher
	cache  SessionCache
	clock  Clock
	log    *slog.Logger

	sessionLifetime time.Duration
	cacheWindow     time.Duration
	stepUpWindow    time.Duration
}

// Options configures a Service. Zero values select the defaults above, so a
// caller cannot accidentally configure a session that never expires or a cache
// entry that outlives its own revocation.
type Options struct {
	SessionLifetime time.Duration
	CacheWindow     time.Duration
	StepUpWindow    time.Duration
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

	return &Service{
		repo:            repo,
		hasher:          hasher,
		cache:           cache,
		clock:           clock,
		log:             log,
		sessionLifetime: opts.SessionLifetime,
		cacheWindow:     opts.CacheWindow,
		stepUpWindow:    opts.StepUpWindow,
	}
}

// StepUpWindow reports how recently a second factor must have been satisfied
// for a sensitive action to proceed.
func (s *Service) StepUpWindow() time.Duration { return s.stepUpWindow }

// errInvalid builds a caller-facing validation error.
func errInvalid(message string) error {
	return apperr.Invalid("account: " + message)
}
