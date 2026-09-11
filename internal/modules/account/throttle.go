package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Throttle policy.
//
// The first few failures cost nothing, because ordinary people mistype
// passwords. After that each further failure doubles the wait, which makes a
// guessing run expensive within seconds while a real person who fumbles twice
// notices nothing.
//
// Note what this is not: an account lockout. Lockout would let anyone who
// knows an address deny its owner access to their own money, and the person
// locked out during an incident is usually the legitimate owner trying to
// revoke an agent. Backoff raises the attacker's cost without handing them a
// remote off switch.
const (
	throttleFreeAttempts = 5
	throttleBaseDelay    = 2 * time.Second
	throttleMaxDelay     = 15 * time.Minute

	// throttleWindow is how long the failure count survives. A counter that
	// never decayed would punish someone for a bad day last month.
	throttleWindow = time.Hour
)

// cacheThrottle implements Throttle over the shared cache.
//
// Two keys per identity: a counter that decays, and a block whose remaining
// lifetime *is* the penalty. Storing the penalty as a key with a TTL means the
// wait is enforced by expiry rather than by arithmetic over a stored
// timestamp, so there is no clock comparison to get wrong.
type cacheThrottle struct {
	cache SessionCache
	inc   Incrementer
	log   *slog.Logger
}

// Incrementer is the atomic counter this throttle needs. It is separate from
// SessionCache because only the throttle requires it.
type Incrementer interface {
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)
}

// NewCacheThrottle builds a Throttle backed by the cache.
func NewCacheThrottle(cache SessionCache, inc Incrementer, log *slog.Logger) Throttle {
	if log == nil {
		log = slog.Default()
	}
	return &cacheThrottle{cache: cache, inc: inc, log: log}
}

// ThrottleKey builds the identity a throttle counts against.
//
// Both the client address and the address being tried are included. Keying on
// the address alone would let anyone lock a stranger out by guessing at their
// login; keying on the client alone would let a botnet spread one guess per
// host across a whole password list unimpeded.
func ThrottleKey(scope, ip, email string) string {
	return fmt.Sprintf("%s|%s|%s", scope, ip, strings.ToLower(email))
}

func (t *cacheThrottle) blockKey(key string) string { return "throttle:block:" + key }
func (t *cacheThrottle) countKey(key string) string { return "throttle:fail:" + key }

// RetryAfter reports the wait still in force, or zero if the caller may
// proceed.
func (t *cacheThrottle) RetryAfter(ctx context.Context, key string) (time.Duration, error) {
	_, found, err := t.cache.Get(ctx, t.blockKey(key))
	if err != nil {
		// Fail open. The alternative turns a cache blip into everybody being
		// unable to sign in, which is worse than the guessing this slows down.
		t.log.Warn("throttle read failed, allowing the attempt",
			slog.String("error", err.Error()))
		return 0, nil
	}
	if !found {
		return 0, nil
	}

	// The block exists, so its remaining lifetime is the wait. A TTL lookup
	// that fails still leaves the caller blocked, just without a precise
	// number to report.
	ttl, err := t.ttl(ctx, t.blockKey(key))
	if err != nil || ttl <= 0 {
		return throttleBaseDelay, nil
	}
	return ttl, nil
}

// Fail records an unsuccessful attempt and returns the penalty now in force.
func (t *cacheThrottle) Fail(ctx context.Context, key string) (time.Duration, error) {
	attempts, err := t.inc.Increment(ctx, t.countKey(key), throttleWindow)
	if err != nil {
		t.log.Warn("throttle increment failed, no penalty applied",
			slog.String("error", err.Error()))
		return 0, nil
	}

	if attempts <= throttleFreeAttempts {
		return 0, nil
	}

	// Each failure past the allowance doubles the wait: 2s, 4s, 8s, and so on
	// up to the cap.
	penalty := throttleBaseDelay << min(attempts-throttleFreeAttempts-1, 32)
	if penalty > throttleMaxDelay || penalty <= 0 {
		penalty = throttleMaxDelay
	}

	// The value is irrelevant; presence and remaining lifetime carry the
	// meaning.
	if err := t.cache.Set(ctx, t.blockKey(key), []byte{1}, penalty); err != nil {
		t.log.Warn("throttle write failed, no penalty applied",
			slog.String("error", err.Error()))
		return 0, nil
	}

	return penalty, nil
}

// Succeed clears both keys, so a correct password immediately restores a
// person to a clean slate.
func (t *cacheThrottle) Succeed(ctx context.Context, key string) error {
	var errs []error
	if err := t.cache.Delete(ctx, t.countKey(key)); err != nil {
		errs = append(errs, err)
	}
	if err := t.cache.Delete(ctx, t.blockKey(key)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ttlReader is the optional capability used to report a precise wait. A cache
// without it still throttles, it just cannot say for how long.
type ttlReader interface {
	TTL(ctx context.Context, key string) (time.Duration, error)
}

func (t *cacheThrottle) ttl(ctx context.Context, key string) (time.Duration, error) {
	reader, ok := t.cache.(ttlReader)
	if !ok {
		return 0, errors.New("account: cache cannot report ttl")
	}
	return reader.TTL(ctx, key)
}
