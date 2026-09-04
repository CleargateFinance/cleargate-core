package ledger

import (
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// PeriodType names the window a spend counter covers.
type PeriodType string

const (
	// PeriodDay tracks spending against a daily cap.
	PeriodDay PeriodType = "day"
	// PeriodMonth tracks spending against a monthly cap.
	PeriodMonth PeriodType = "month"
)

// Valid reports whether p is a recognised period type.
func (p PeriodType) Valid() bool {
	return p == PeriodDay || p == PeriodMonth
}

// Period identifies one counter window: a type, and the date it starts on.
type Period struct {
	Type  PeriodType
	Start time.Time
}

// DayPeriod returns the daily window containing at.
//
// Windows are computed in UTC so a counter does not shift when a server's
// local time zone changes, and so two instances in different regions agree on
// which window a payment falls into.
func DayPeriod(at time.Time) Period {
	utc := at.UTC()
	return Period{
		Type:  PeriodDay,
		Start: time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC),
	}
}

// MonthPeriod returns the monthly window containing at.
func MonthPeriod(at time.Time) Period {
	utc := at.UTC()
	return Period{
		Type:  PeriodMonth,
		Start: time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC),
	}
}

// PeriodsFor returns every window a payment made at this instant counts
// against, which is the set the authorization path must check.
func PeriodsFor(at time.Time) []Period {
	return []Period{DayPeriod(at), MonthPeriod(at)}
}

// End returns the first instant after the window closes.
func (p Period) End() time.Time {
	switch p.Type {
	case PeriodDay:
		return p.Start.AddDate(0, 0, 1)
	case PeriodMonth:
		return p.Start.AddDate(0, 1, 0)
	default:
		return p.Start
	}
}

// Spend is how much one agent has spent in one window.
type Spend struct {
	AgentID id.AgentID
	Period  Period
	Asset   currency.Asset
	Amount  currency.Amount
}

// Drift is a mismatch between a stored counter and the value recomputed from
// the postings it should have been derived from.
type Drift struct {
	AgentID    id.AgentID
	Period     Period
	Asset      currency.Asset
	Counter    currency.Amount
	Recomputed currency.Amount
}
