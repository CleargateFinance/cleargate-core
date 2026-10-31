package authz

import (
	"slices"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// FeaturesVersion versions the Features encoding, stored as "v" in
// decision.features (data-architecture.md §5.3). Any change to the JSON shape
// below bumps it, and Inputs keeps reading every earlier version.
const FeaturesVersion = 1

// Features is the decision-time snapshot: everything Decide read, plus the
// signals the agent reported, as they were at that instant.
//
// It has one test of completeness: Inputs must rebuild a request and snapshot
// that Decide turns into the same decision. That is what lets a dispute months
// later show the decision rather than argue about it (§7.1), and what lets a
// proposed mandate be evaluated against past traffic.
//
// It holds facts, not derived values. The spend totals are the counters as
// read, not the projected totals Decide computed from them, so the record
// cannot drift from the inputs it claims to come from.
type Features struct {
	V              int              `json:"v"`
	Now            time.Time        `json:"now"`
	Request        RequestFeatures  `json:"request"`
	Mandate        *MandateFeatures `json:"mandate"`
	AccountBlocked bool             `json:"account_blocked"`
	DaySpent       AmountFeature    `json:"day_spent"`
	MonthSpent     AmountFeature    `json:"month_spent"`
	AccountBalance AmountFeature    `json:"account_balance"`
}

// RequestFeatures is the request as the engine received it.
type RequestFeatures struct {
	Amount   AmountFeature `json:"amount"`
	Category string        `json:"category"`
	PayTo    string        `json:"pay_to"`

	ResourceURL                string    `json:"resource_url,omitempty"`
	URLSource                  URLSource `json:"url_source"`
	Purpose                    string    `json:"purpose,omitempty"`
	Task                       string    `json:"task,omitempty"`
	SessionID                  string    `json:"session_id,omitempty"`
	SessionNovelCounterparties *int      `json:"session_novel_counterparties,omitempty"`
}

// MandateFeatures is the rules in force. Null when the agent had none.
//
// The mandate's identity, version and digest are columns of the decision
// record itself; this is the content the engine evaluated.
type MandateFeatures struct {
	Asset       currency.Asset `json:"asset"`
	MaxPerTx    AmountFeature  `json:"max_per_tx"`
	MaxPerDay   AmountFeature  `json:"max_per_day"`
	MaxPerMonth AmountFeature  `json:"max_per_month"`
	Categories  []string       `json:"categories"`
	PayTo       []string       `json:"pay_to"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

// AmountFeature is an amount with its asset kept beside it, never assumed.
// A decision that declined because two inputs were in different assets must
// replay to the same decline, so each amount carries its own.
type AmountFeature struct {
	Value string         `json:"value"`
	Asset currency.Asset `json:"asset"`
}

func buildFeatures(req Request, s Snapshot) Features {
	src := req.Signals.URLSource
	if src == "" {
		src = URLSourceUnknown
	}

	f := Features{
		V:   FeaturesVersion,
		Now: s.Now.UTC(),
		Request: RequestFeatures{
			Amount:                     amountFeature(req.Amount),
			Category:                   req.Category,
			PayTo:                      req.PayTo,
			ResourceURL:                req.Signals.ResourceURL,
			URLSource:                  src,
			Purpose:                    req.Signals.Purpose,
			Task:                       req.Signals.Task,
			SessionID:                  req.Signals.SessionID,
			SessionNovelCounterparties: cloneInt(req.Signals.SessionNovelCounterparties),
		},
		AccountBlocked: s.AccountBlocked,
		DaySpent:       amountFeature(s.DaySpent),
		MonthSpent:     amountFeature(s.MonthSpent),
		AccountBalance: amountFeature(s.AccountBalance),
	}

	// Copied, so a caller that reuses the rules' slices afterwards cannot
	// change what the record says was evaluated.
	if m := s.Mandate; m != nil {
		f.Mandate = &MandateFeatures{
			Asset:       m.Asset,
			MaxPerTx:    amountFeature(m.MaxPerTx),
			MaxPerDay:   amountFeature(m.MaxPerDay),
			MaxPerMonth: amountFeature(m.MaxPerMonth),
			Categories:  slices.Clone(m.Categories),
			PayTo:       slices.Clone(m.PayTo),
			ExpiresAt:   m.ExpiresAt.UTC(),
		}
	}

	return f
}

// Inputs rebuilds the request and snapshot a decision was made from. Decide
// over them gives the recorded decision again.
func (f Features) Inputs() (Request, Snapshot, error) {
	if f.V != FeaturesVersion {
		return Request{}, Snapshot{}, apperr.Internal("authz: unknown features version", nil)
	}

	var err error
	amount := func(a AmountFeature) currency.Amount {
		v, e := a.amount()
		if e != nil && err == nil {
			err = e
		}
		return v
	}

	req := Request{
		Amount:   amount(f.Request.Amount),
		Category: f.Request.Category,
		PayTo:    f.Request.PayTo,
		Signals: Signals{
			ResourceURL:                f.Request.ResourceURL,
			URLSource:                  f.Request.URLSource,
			Purpose:                    f.Request.Purpose,
			Task:                       f.Request.Task,
			SessionID:                  f.Request.SessionID,
			SessionNovelCounterparties: cloneInt(f.Request.SessionNovelCounterparties),
		},
	}
	s := Snapshot{
		AccountBlocked: f.AccountBlocked,
		DaySpent:       amount(f.DaySpent),
		MonthSpent:     amount(f.MonthSpent),
		AccountBalance: amount(f.AccountBalance),
		Now:            f.Now,
	}
	if m := f.Mandate; m != nil {
		s.Mandate = &Rules{
			Asset:       m.Asset,
			MaxPerTx:    amount(m.MaxPerTx),
			MaxPerDay:   amount(m.MaxPerDay),
			MaxPerMonth: amount(m.MaxPerMonth),
			Categories:  slices.Clone(m.Categories),
			PayTo:       slices.Clone(m.PayTo),
			ExpiresAt:   m.ExpiresAt,
		}
	}

	if err != nil {
		return Request{}, Snapshot{}, err
	}
	return req, s, nil
}

func amountFeature(a currency.Amount) AmountFeature {
	return AmountFeature{Value: a.String(), Asset: a.Asset()}
}

// amount reverses amountFeature. An empty asset is the zero Amount, which
// Decide declines with processing_error, so it must replay as that and not
// fail to parse.
func (a AmountFeature) amount() (currency.Amount, error) {
	if a.Asset == "" {
		var zero currency.Amount
		if a.Value != zero.String() {
			return currency.Amount{}, apperr.Internal("authz: features amount has a value but no asset", nil)
		}
		return currency.Amount{}, nil
	}
	v, err := currency.Parse(a.Value, a.Asset)
	if err != nil {
		return currency.Amount{}, apperr.Internal("authz: features amount is unreadable", err)
	}
	return v, nil
}

func cloneInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
