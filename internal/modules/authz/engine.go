package authz

import (
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// Decide evaluates a payment request against pre-fetched state.
//
// PURE BY CONTRACT: no context, no error, no I/O, and no clock other than
// s.Now. Every input arrives in the arguments. See doc.go for why.
//
// It fails closed. currency.Amount refuses to compare or add across assets,
// and every such refusal here declines, so an input in the wrong asset can
// never slip past a cap.
func Decide(req Request, s Snapshot) Decision {
	// Built first, from the untouched inputs, so a decline at any stage is
	// recorded with everything the engine was given.
	e := evaluation{features: buildFeatures(req, s)}

	if req.Amount.IsZero() || req.Amount.IsNegative() {
		return e.decline(StageRequest, ReasonInvalidAmount, "amount must be positive")
	}
	e.pass(StageRequest)

	if s.AccountBlocked {
		return e.decline(StageAccount, ReasonAccountBlocked, "account is suspended or frozen")
	}
	e.pass(StageAccount)

	if code, reason := checkMandate(req, s); code != ReasonOK {
		return e.decline(StageMandate, code, reason)
	}
	e.pass(StageMandate)

	// MVP: counterparty, intent and behaviour checks do not exist yet (§22).
	e.skip(StageCounterparty)
	e.skip(StageIntent)
	e.skip(StageBehaviour)

	if s.AccountBalance.Asset() != req.Amount.Asset() {
		return e.decline(StageFunds, ReasonProcessingError, "account balance is in another asset")
	}
	if exceeds(req.Amount, s.AccountBalance) {
		return e.decline(StageFunds, ReasonInsufficientFunds, "insufficient account balance")
	}
	e.pass(StageFunds)

	return e.approve()
}

// checkMandate runs the mandate stage (§6.1), returning ReasonOK when the
// payment is within the agent's authority.
func checkMandate(req Request, s Snapshot) (ReasonCode, string) {
	m := s.Mandate
	if m == nil {
		return ReasonNoMandate, "agent has no mandate in force"
	}
	if !s.Now.Before(m.ExpiresAt) {
		return ReasonMandateExpired, "mandate expired"
	}
	if req.Amount.Asset() != m.Asset {
		return ReasonAssetNotPermitted,
			"mandate permits " + string(m.Asset) + ", not " + string(req.Amount.Asset())
	}

	// From here every amount must be in the mandate's asset. A mismatch is a
	// bug in whatever built the snapshot, and it is caught before the first
	// comparison so it cannot be mistaken for a cap breach or a pass.
	for _, a := range []currency.Amount{m.MaxPerTx, m.MaxPerDay, m.MaxPerMonth, s.DaySpent, s.MonthSpent} {
		if a.Asset() != m.Asset {
			return ReasonProcessingError, "decision inputs are in different assets"
		}
	}

	if !m.PermitsCategory(req.Category) {
		return ReasonCategoryNotPermitted, "category " + quote(req.Category) + " is not permitted by the mandate"
	}
	if !m.PermitsPayee(req.PayTo) {
		return ReasonPayeeNotPermitted, "payee is not on the mandate's allowlist"
	}
	if exceeds(req.Amount, m.MaxPerTx) {
		return ReasonOverPerTxCap, "over the " + m.MaxPerTx.String() + " " + string(m.Asset) + " per-transaction limit"
	}

	day, err := s.DaySpent.Add(req.Amount)
	if err != nil {
		return ReasonProcessingError, "daily spend could not be totalled"
	}
	if exceeds(day, m.MaxPerDay) {
		return ReasonOverDailyCap, "would exceed the " + m.MaxPerDay.String() + " " + string(m.Asset) + " daily limit"
	}

	month, err := s.MonthSpent.Add(req.Amount)
	if err != nil {
		return ReasonProcessingError, "monthly spend could not be totalled"
	}
	if exceeds(month, m.MaxPerMonth) {
		return ReasonOverMonthlyCap, "would exceed the " + m.MaxPerMonth.String() + " " + string(m.Asset) + " monthly limit"
	}

	return ReasonOK, ""
}

// exceeds reports a > limit, treating amounts it cannot compare as exceeding.
func exceeds(a, limit currency.Amount) bool {
	over, err := a.GreaterThan(limit)
	return err != nil || over
}

// quote bounds what the agent's own input contributes to the reason text,
// which the console displays.
func quote(s string) string {
	const limit = 64
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return `"` + s + `"`
}

// evaluation accumulates the stages that ran.
type evaluation struct {
	features Features
	checks   []Check
}

func (e *evaluation) pass(st Stage) { e.checks = append(e.checks, Check{Stage: st, Result: Pass}) }
func (e *evaluation) skip(st Stage) { e.checks = append(e.checks, Check{Stage: st, Result: Skipped}) }

func (e *evaluation) decline(st Stage, code ReasonCode, reason string) Decision {
	e.checks = append(e.checks, Check{Stage: st, Result: Fail})
	return Decision{Outcome: Declined, ReasonCode: code, Reason: reason, Checks: e.checks, Features: e.features}
}

func (e *evaluation) approve() Decision {
	return Decision{Outcome: Approved, ReasonCode: ReasonOK, Reason: "within mandate", Checks: e.checks, Features: e.features}
}
