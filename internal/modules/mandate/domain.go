package mandate

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Bounds on what a mandate may list. Generous for any real policy, small
// enough that a console endpoint cannot be handed a megabyte to store and the
// authorization path never scans a long list per payment.
const (
	MaxCategories = 32
	MaxPayees     = 64

	// MaxPayeeLength fits every address format the rails use, with room.
	MaxPayeeLength = 128

	// MaxCapDecimalPlaces matches the storage column, numeric(38,18). A cap
	// finer than that would be stored as a different number from the one
	// approved, and the database refuses it too.
	MaxCapDecimalPlaces = 18
)

// categoryPattern is what a category may look like: a lower-case slug.
// Categories are classes ("data_api"), never instances (§6.2), and keeping
// them to this alphabet is also what lets the payload encoding need no
// escaping, see payload.go.
var categoryPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Status is where a mandate is in its lifecycle.
type Status string

const (
	// StatusPending is proposed and waiting out the account's timelock. The
	// MVP never writes it: with no guardian there is nobody to wait for
	// (Task 2.8), so a mandate goes straight to active.
	StatusPending Status = "pending"

	// StatusActive is the version in force. At most one exists per agent.
	StatusActive Status = "active"

	// StatusVetoed was stopped by the guardian and never took effect.
	StatusVetoed Status = "vetoed"

	// StatusSuperseded was in force and has been replaced by a newer version.
	// It stays, because a payment made under it is judged against it.
	StatusSuperseded Status = "superseded"
)

// SignatureScheme records how a mandate was approved.
type SignatureScheme string

const (
	// SchemeSessionStepUp is approval by a console session with a recent TOTP
	// step-up. It is the MVP's stand-in for a user-held signature and carries
	// no assertion, so it proves who was signed in, not what they saw.
	SchemeSessionStepUp SignatureScheme = "session_stepup"

	// SchemeWebAuthn is approval by a passkey signing the payload digest
	// (Task 3.1). Not issued yet.
	SchemeWebAuthn SignatureScheme = "webauthn"
)

// Rules are the limits a mandate grants an agent.
//
// Every cap is in the one asset the mandate names. Spending in any other
// asset finds no mandate, rather than being converted, because a conversion
// rate is a fact nobody approved.
type Rules struct {
	Asset       currency.Asset
	MaxPerTx    currency.Amount
	MaxPerDay   currency.Amount
	MaxPerMonth currency.Amount

	// Categories the agent may spend in. At least one: a mandate that permits
	// nothing is a revocation, and revoking is done on the agent.
	Categories []string

	// PayTo, when non-empty, is the only set of payees the agent may pay. Empty
	// permits any payee, since an agent discovers most of what it buys at
	// runtime (§6.2). Addresses are kept exactly as given: whether case is
	// significant depends on the chain, and only the rail knows.
	PayTo []string

	// ExpiresAt is when the authority lapses on its own, whatever else
	// happens. Kept to the second, which is what the payload records.
	ExpiresAt time.Time
}

// normalised returns the rules in the one form they are stored and signed in:
// lists sorted and de-duplicated, expiry in UTC to the second. Two requests
// that mean the same thing therefore produce the same bytes.
func (r Rules) normalised() Rules {
	r.Categories = sortedUnique(r.Categories)
	r.PayTo = sortedUnique(r.PayTo)
	r.ExpiresAt = r.ExpiresAt.UTC().Truncate(time.Second)
	return r
}

func sortedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.TrimSpace(s))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Validate reports whether the rules may be issued at now.
//
// The database checks the cap ordering, the expiry and the category count
// again, so no other write path can store a mandate these checks would
// refuse. These exist for the message.
func (r Rules) Validate(now time.Time) error {
	if r.Asset == "" {
		return errInvalid("an asset is required")
	}
	// The unit of account. A mandate in anything else would be refused by
	// the engine on every payment (asset_not_permitted), so it is refused
	// here, where the person setting it can be told why.
	if r.Asset != currency.USD {
		return errInvalid("mandates are in " + string(currency.USD))
	}

	caps := []struct {
		name   string
		amount currency.Amount
	}{
		{"per-transaction", r.MaxPerTx},
		{"daily", r.MaxPerDay},
		{"monthly", r.MaxPerMonth},
	}
	for _, c := range caps {
		if c.amount.Asset() != r.Asset {
			return errInvalid("the " + c.name + " cap must be in " + string(r.Asset))
		}
		if c.amount.IsZero() || c.amount.IsNegative() {
			return errInvalid("the " + c.name + " cap must be positive")
		}
		if d := c.amount.Decimal(); !d.Equal(d.Truncate(MaxCapDecimalPlaces)) {
			return errInvalid("the " + c.name + " cap has too many decimal places")
		}
	}
	if over, _ := r.MaxPerTx.GreaterThan(r.MaxPerDay); over {
		return errInvalid("the per-transaction cap cannot exceed the daily cap")
	}
	if over, _ := r.MaxPerDay.GreaterThan(r.MaxPerMonth); over {
		return errInvalid("the daily cap cannot exceed the monthly cap")
	}

	if len(r.Categories) == 0 {
		return errInvalid("at least one category is required")
	}
	if len(r.Categories) > MaxCategories {
		return errInvalid("too many categories")
	}
	for _, c := range r.Categories {
		if !categoryPattern.MatchString(c) {
			return errInvalid("category " + quote(c) + " must be a lower-case slug")
		}
	}

	if len(r.PayTo) > MaxPayees {
		return errInvalid("too many payees")
	}
	for _, p := range r.PayTo {
		if p == "" || len(p) > MaxPayeeLength || strings.ContainsFunc(p, notAddressChar) {
			return errInvalid("payee " + quote(p) + " is not an address")
		}
	}

	if r.ExpiresAt.IsZero() {
		return errInvalid("an expiry is required")
	}
	if !r.ExpiresAt.After(now) {
		return errInvalid("the expiry must be in the future")
	}

	return nil
}

// notAddressChar reports a character no address format uses. Every format the
// rails speak (base58, hex, bech32) is printable ASCII, and holding payees to
// it is what keeps the payload encoding free of escapes, see payload.go.
func notAddressChar(r rune) bool { return r <= ' ' || r > '~' }

// quote bounds what a caller's own input contributes to an error message.
func quote(s string) string {
	const limit = 64
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return `"` + s + `"`
}

// Mandate is one version of an agent's spending authority.
type Mandate struct {
	ID      id.MandateID
	AgentID id.AgentID
	Version int

	// Payload is the exact bytes approved. Rules are parsed from it and never
	// encoded back into it, see payload.go.
	Payload []byte

	// Digest is sha256 of Payload. A decision records it, so the record names
	// the exact authority it was checked against, not just a row that could
	// in principle be reinterpreted.
	Digest []byte

	Rules Rules

	Scheme   SignatureScheme
	SignedBy id.UserID

	Status        Status
	EffectiveFrom time.Time
	EffectiveTo   *time.Time
	CreatedAt     time.Time
}

// Draft is a mandate about to be stored. The database assigns its identifier
// and derives the digest and projections from the payload.
type Draft struct {
	AgentID       id.AgentID
	Version       int
	Payload       []byte
	Scheme        SignatureScheme
	SignedBy      id.UserID
	Status        Status
	EffectiveFrom time.Time
}

// Agent is what this module needs to know about the agent a mandate is for.
type Agent struct {
	ID        id.AgentID
	AccountID id.AccountID
	Active    bool
}

// Actor is the person issuing a mandate, as their console session describes
// them. The account module decides from it whether they may.
type Actor struct {
	UserID         id.UserID
	MFASatisfiedAt *time.Time
}

// Errors reported by this module.
var (
	// ErrNoMandate reports an agent with no mandate in force at the instant
	// asked about. An agent without one cannot spend.
	ErrNoMandate = apperr.NotFound("agent has no mandate in force")

	// ErrAgentRevoked reports a mandate issued to a retired agent. Authority
	// granted to an identity that can no longer authenticate is unusable, and
	// would come back to life if anything ever reactivated it.
	ErrAgentRevoked = apperr.Conflict("this agent has been revoked")

	// ErrConcurrentChange reports two mandates issued to one agent at once.
	// Neither silently wins: the caller reloads and decides again, since the
	// rules it was replacing are no longer the rules in force.
	ErrConcurrentChange = apperr.Conflict("the agent's mandate changed while this one was being issued, reload and try again")

	// ErrUnverifiable reports a stored mandate this build cannot verify. It is
	// refused rather than trusted, see Service.Current.
	ErrUnverifiable = apperr.Internal("mandate approval cannot be verified", nil)
)

func errInvalid(message string) error {
	return apperr.Invalid("mandate: " + message)
}
