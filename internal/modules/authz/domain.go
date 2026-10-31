package authz

import (
	"slices"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// Outcome is what the engine decided. The decision record also allows held
// and escalated (data-architecture.md §5.3); nothing produces them until the
// counterparty and intent checks exist.
type Outcome string

const (
	// Approved lets the payment proceed.
	Approved Outcome = "approved"
	// Declined stops it. Nothing is charged.
	Declined Outcome = "declined"
)

// ReasonCode says why a payment was approved or declined.
//
// The codes are a public contract: they are published in api/openapi.yaml,
// SDKs branch on them, and two years of decision records aggregate on them.
// Renaming one is a breaking change, so the human-readable text lives in
// Decision.Reason, which can be reworded freely (data-architecture.md §5.3).
//
// Decide checks them in the order declared here and stops at the first
// failure, so a payment that breaks several rules reports the earliest. That
// order is part of the contract too.
type ReasonCode string

const (
	// ReasonOK accompanies every approval.
	ReasonOK ReasonCode = "ok"

	// ReasonInvalidAmount is an amount of zero or less.
	ReasonInvalidAmount ReasonCode = "invalid_amount"
	// ReasonAccountBlocked is a suspended or frozen account.
	ReasonAccountBlocked ReasonCode = "account_blocked"
	// ReasonNoMandate is an agent with no mandate in force. It is a decline
	// and not an error: an agent without authority trying to pay is exactly
	// the attempt the console exists to show (§12.1).
	ReasonNoMandate ReasonCode = "no_mandate"
	// ReasonMandateExpired is a mandate whose expiry has passed.
	ReasonMandateExpired ReasonCode = "mandate_expired"
	// ReasonAssetNotPermitted is a payment in an asset the mandate does not
	// name. It is never converted, since a rate is a fact nobody approved.
	ReasonAssetNotPermitted ReasonCode = "asset_not_permitted"
	// ReasonCategoryNotPermitted is a category the mandate does not list.
	ReasonCategoryNotPermitted ReasonCode = "category_not_permitted"
	// ReasonPayeeNotPermitted is a payee missing from a non-empty allowlist.
	ReasonPayeeNotPermitted ReasonCode = "payee_not_permitted"
	// ReasonOverPerTxCap is an amount above the per-transaction cap.
	ReasonOverPerTxCap ReasonCode = "over_per_tx_cap"
	// ReasonOverDailyCap is a payment that would take today's spend past the
	// daily cap.
	ReasonOverDailyCap ReasonCode = "over_daily_cap"
	// ReasonOverMonthlyCap is a payment that would take this month's spend
	// past the monthly cap.
	ReasonOverMonthlyCap ReasonCode = "over_monthly_cap"
	// ReasonInsufficientFunds is an account balance that does not cover the
	// payment. Checked last: the balance is shared by every agent on the
	// account, so it is the least specific thing a decline can say.
	ReasonInsufficientFunds ReasonCode = "insufficient_funds"

	// ReasonProcessingError means the inputs disagreed with each other, for
	// example a spend counter in a different asset from the mandate. That is
	// a Cleargate bug rather than anything the agent did, and it declines
	// because an engine that cannot compare two amounts must not guess.
	ReasonProcessingError ReasonCode = "processing_error"
)

// Stage names one step of the §6 pipeline.
//
// Counterparty, intent and behaviour are the three checks §22 leaves out of
// the MVP. Decide records them as skipped, so the decision record has its
// final shape now and filling one in later is a function body (§6.5).
type Stage string

const (
	// StageRequest checks the request is well formed.
	StageRequest Stage = "request"
	// StageAccount checks the account may spend at all.
	StageAccount Stage = "account"
	// StageMandate checks the payment is within the agent's authority (§6.1).
	StageMandate Stage = "mandate"
	// StageCounterparty scores the payee (§6.2). Not built.
	StageCounterparty Stage = "counterparty"
	// StageIntent compares the payment with the agent's task (§6.3). Not built.
	StageIntent Stage = "intent"
	// StageBehaviour compares it with the agent's history (§6.4). Not built.
	StageBehaviour Stage = "behaviour"
	// StageFunds checks the account's balance covers the payment.
	StageFunds Stage = "funds"
)

// Result is how one stage went.
type Result string

const (
	// Pass is a stage that ran and found nothing wrong.
	Pass Result = "pass"
	// Fail is the stage that declined the payment.
	Fail Result = "fail"

	// Skipped is a check that did not run. It is not recorded as a pass: a
	// decision record read during a dispute must not claim a counterparty was
	// checked when nothing checked it.
	Skipped Result = "skipped"
)

// Check is one stage's result, one decision_check row.
type Check struct {
	Stage  Stage
	Result Result
}

// Rules are the mandate limits the engine evaluates against. The payment
// module copies them from the mandate in force; this module never reads a
// mandate itself, so the engine depends on what a mandate says and not on
// how one is stored or approved.
type Rules struct {
	Asset       currency.Asset
	MaxPerTx    currency.Amount
	MaxPerDay   currency.Amount
	MaxPerMonth currency.Amount
	Categories  []string

	// PayTo, when non-empty, is the only set of payees permitted. Compared
	// exactly, as the mandate stores them.
	PayTo []string

	// ExpiresAt is the instant the authority lapses. At ExpiresAt itself the
	// mandate is already expired.
	ExpiresAt time.Time
}

// PermitsCategory reports whether the mandate allows spending in c.
func (r Rules) PermitsCategory(c string) bool { return slices.Contains(r.Categories, c) }

// PermitsPayee reports whether the mandate allows paying p. An empty
// allowlist permits any payee, since an agent finds most of what it buys at
// runtime (§6.2).
func (r Rules) PermitsPayee(p string) bool {
	return len(r.PayTo) == 0 || slices.Contains(r.PayTo, p)
}

// Request is the payment an agent asked for.
type Request struct {
	Amount   currency.Amount
	Category string
	PayTo    string
}

// Snapshot is every input Decide reads besides the request, fetched before it
// is called.
//
// Every amount must be in the mandate's asset. A counter with no spend yet is
// currency.Zero of that asset, not the zero Amount, which has no asset and so
// declines as a processing error.
type Snapshot struct {
	// Mandate is the rules in force, or nil when the agent has none.
	Mandate *Rules

	// AccountBlocked is true when the account is suspended or frozen.
	AccountBlocked bool

	// DaySpent and MonthSpent are the agent's spend in the current UTC
	// calendar day and month, before this payment (Task 1.3).
	DaySpent   currency.Amount
	MonthSpent currency.Amount

	// AccountBalance is the account's spendable funds. Money is pooled per
	// account, so this is shared by all its agents.
	AccountBalance currency.Amount

	// Now is the decision instant, from the injected clock.
	Now time.Time
}

// Decision is what Decide returns.
type Decision struct {
	Outcome    Outcome
	ReasonCode ReasonCode

	// Reason is the human-readable explanation, for the console.
	Reason string

	// Checks are the stages that ran, in order. Evaluation stops at the first
	// failure, so a decline's last check is the one that failed.
	Checks []Check
}

// Approved reports whether the payment may proceed.
func (d Decision) Approved() bool { return d.Outcome == Approved }
