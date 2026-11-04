package summary

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// The only file in this module that knows Gin exists.
// Handlers decode, delegate to Service, and encode. No business logic here.

// RegisterConsoleRoutes adds the summary. Pass it the console group: it is a
// person's view of their account, and an agent has no business reading the
// balance it draws on or the caps it is held to (§6.1).
func (m *Module) RegisterConsoleRoutes(rg *gin.RouterGroup) {
	rg.GET("/accounts/:id/summary", m.account)
}

// errNotPermitted is the account module's answer to a non-member, word for
// word, so an account id that is not a UUID, one that names nothing and one
// that is somebody else's cannot be told apart.
var errNotPermitted = apperr.Forbidden("your role does not permit this change")

type summaryResponse struct {
	Account accountJSON `json:"account"`

	// Balance is at par: a claim on USDC one for one. No market value is
	// shown until there is a price to show (doc.go).
	Balance amountJSON `json:"balance"`

	DayStart   time.Time `json:"day_start"`
	MonthStart time.Time `json:"month_start"`

	Agents []agentJSON `json:"agents"`

	// DeclinedThisMonth is the account's blocked count since month_start.
	DeclinedThisMonth declinesJSON `json:"declined_this_month"`
}

type accountJSON struct {
	ID       id.AccountID `json:"id"`
	Name     string       `json:"name"`
	Role     string       `json:"role"`
	FrozenAt *time.Time   `json:"frozen_at"`
}

type agentJSON struct {
	ID     id.AgentID `json:"id"`
	Name   string     `json:"name"`
	Status string     `json:"status"`

	// Mandate is null when the agent has none, and so cannot spend.
	Mandate *capsJSON `json:"mandate"`

	SpentToday        string       `json:"spent_today"`
	SpentThisMonth    string       `json:"spent_this_month"`
	DeclinedThisMonth declinesJSON `json:"declined_this_month"`
}

// capsJSON are the mandate's limits. Amounts are decimal strings, never JSON
// numbers, as everywhere money crosses the wire.
type capsJSON struct {
	ID          id.MandateID   `json:"id"`
	Version     int            `json:"version"`
	Asset       currency.Asset `json:"asset"`
	MaxPerTx    string         `json:"max_per_tx"`
	MaxPerDay   string         `json:"max_per_day"`
	MaxPerMonth string         `json:"max_per_month"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

type declinesJSON struct {
	Count int `json:"count"`

	// Amounts is one total per asset the declined payments asked for.
	Amounts []amountJSON `json:"amounts"`
}

type amountJSON struct {
	Amount string         `json:"amount"`
	Asset  currency.Asset `json:"asset"`
}

func (m *Module) account(c *gin.Context) {
	user, ok := caller.UserFrom(c.Request.Context())
	if !ok {
		// Registered outside the console group. A wiring defect, and the
		// summary must not be served to nobody in particular.
		abort(c, apperr.Internal("summary: route reached without a console session", nil))
		return
	}

	accountID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		abort(c, errNotPermitted)
		return
	}

	sum, err := m.svc.Account(c.Request.Context(), Actor{UserID: user.UserID}, id.AccountID(accountID.String()))
	if err != nil {
		abort(c, err)
		return
	}

	out := summaryResponse{
		Account: accountJSON{
			ID: sum.Account.ID, Name: sum.Account.Name, Role: sum.Account.Role, FrozenAt: sum.Account.FrozenAt,
		},
		Balance:           wireAmount(sum.Balance),
		DayStart:          sum.DayStart,
		MonthStart:        sum.MonthStart,
		Agents:            make([]agentJSON, len(sum.Agents)),
		DeclinedThisMonth: wireDeclines(sum.Declined),
	}
	for i, a := range sum.Agents {
		out.Agents[i] = agentJSON{
			ID: a.ID, Name: a.Name, Status: a.Status,
			SpentToday:        a.SpentToday.String(),
			SpentThisMonth:    a.SpentThisMonth.String(),
			DeclinedThisMonth: wireDeclines(a.Declined),
		}
		if caps := a.Mandate; caps != nil {
			out.Agents[i].Mandate = &capsJSON{
				ID: caps.ID, Version: caps.Version, Asset: caps.MaxPerTx.Asset(),
				MaxPerTx: caps.MaxPerTx.String(), MaxPerDay: caps.MaxPerDay.String(),
				MaxPerMonth: caps.MaxPerMonth.String(), ExpiresAt: caps.ExpiresAt,
			}
		}
	}
	c.JSON(http.StatusOK, out)
}

func wireAmount(a currency.Amount) amountJSON {
	return amountJSON{Amount: a.String(), Asset: a.Asset()}
}

func wireDeclines(d Declines) declinesJSON {
	out := declinesJSON{Count: d.Count, Amounts: make([]amountJSON, len(d.Amounts))}
	for i, a := range d.Amounts {
		out.Amounts[i] = wireAmount(a)
	}
	return out
}

// abort records err for server.ErrorMapper, which writes the response, and
// stops the chain.
func abort(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}
