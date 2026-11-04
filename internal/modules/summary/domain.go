package summary

import (
	"slices"
	"strings"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Summary is one account at one instant.
type Summary struct {
	Account Account

	// Balance is the account's customer funds, in the unit of account.
	Balance currency.Amount

	// DayStart and MonthStart bound the windows every figure below is in.
	DayStart   time.Time
	MonthStart time.Time

	Agents []Agent

	// Declined is every agent's declines this month, added up. The headline
	// figure of §12.1, and the one thing here that exists nowhere else.
	Declined Declines
}

// Account is the account as its member sees it.
type Account struct {
	ID   id.AccountID
	Name string

	// Role is what the reader may do here, so the console can offer only
	// what will be allowed.
	Role string

	// FrozenAt is set while the account's owners have stopped all spending.
	FrozenAt *time.Time
}

// Agent is one agent's line: its caps and how much of each is used.
type Agent struct {
	ID     id.AgentID
	Name   string
	Status string

	// Mandate is the caps in force, or nil when the agent has none and so
	// cannot spend at all.
	Mandate *Caps

	SpentToday     currency.Amount
	SpentThisMonth currency.Amount

	Declined Declines
}

// Caps are the limits of the mandate in force.
type Caps struct {
	ID          id.MandateID
	Version     int
	MaxPerTx    currency.Amount
	MaxPerDay   currency.Amount
	MaxPerMonth currency.Amount
	ExpiresAt   time.Time
}

// Declines counts declined payments and totals what they asked for.
type Declines struct {
	Count int

	// Amounts is one total per asset asked for, in asset order. Declines can
	// name an asset the mandate does not permit, and adding those to dollars
	// would invent a figure. An invalid amount is counted and adds nothing.
	Amounts []currency.Amount
}

// add folds another set of declines into d, keeping one total per asset.
func (d Declines) add(other Declines) (Declines, error) {
	out := Declines{Count: d.Count + other.Count, Amounts: append([]currency.Amount(nil), d.Amounts...)}
	for _, a := range other.Amounts {
		merged := false
		for i, have := range out.Amounts {
			if have.Asset() == a.Asset() {
				sum, err := have.Add(a)
				if err != nil {
					return Declines{}, err
				}
				out.Amounts[i], merged = sum, true
				break
			}
		}
		if !merged {
			out.Amounts = append(out.Amounts, a)
		}
	}
	slices.SortFunc(out.Amounts, func(a, b currency.Amount) int {
		return strings.Compare(string(a.Asset()), string(b.Asset()))
	})
	return out, nil
}

// Actor is the person reading the summary, as their console session
// describes them.
type Actor struct {
	UserID id.UserID
}
