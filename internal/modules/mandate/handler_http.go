package mandate

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

// RegisterConsoleRoutes adds the mandate routes. Pass it the console group
// and nothing else: these routes change what an agent may spend, so they
// exist only where a person's session is required (Task 6.1, §6.1).
func (m *Module) RegisterConsoleRoutes(rg *gin.RouterGroup) {
	rg.PUT("/agents/:id/mandate", m.issue)
}

// rulesBody is a mandate's rules on the wire. Amounts are decimal strings,
// never JSON numbers, so no client's float parsing can change a cap.
type rulesBody struct {
	Asset       currency.Asset `json:"asset"`
	MaxPerTx    string         `json:"max_per_tx"`
	MaxPerDay   string         `json:"max_per_day"`
	MaxPerMonth string         `json:"max_per_month"`
	Categories  []string       `json:"categories"`
	PayTo       []string       `json:"pay_to"`
	ExpiresAt   time.Time      `json:"expires_at"`
}

type mandateResponse struct {
	ID            id.MandateID `json:"id"`
	AgentID       id.AgentID   `json:"agent_id"`
	Version       int          `json:"version"`
	Rules         rulesBody    `json:"rules"`
	EffectiveFrom time.Time    `json:"effective_from"`
}

// issue replaces an agent's mandate with a new version. The caller's role and
// recent step-up are checked by the service, through the account module.
func (m *Module) issue(c *gin.Context) {
	user, ok := caller.UserFrom(c.Request.Context())
	if !ok {
		// Registered outside the console group. A wiring defect, and the route
		// must not run without the person who approved it.
		abort(c, apperr.Internal("mandate: route reached without a console session", nil))
		return
	}

	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		abort(c, apperr.NotFound("agent not found"))
		return
	}

	var body rulesBody
	if err := c.ShouldBindJSON(&body); err != nil {
		abort(c, apperr.Invalid("the body must be a mandate's rules as JSON"))
		return
	}
	rules, err := body.rules()
	if err != nil {
		abort(c, err)
		return
	}

	issued, err := m.svc.Issue(c.Request.Context(),
		Actor{UserID: user.UserID, MFASatisfiedAt: user.MFASatisfiedAt},
		id.AgentID(agentID.String()), rules)
	if err != nil {
		abort(c, err)
		return
	}

	c.JSON(http.StatusOK, mandateResponse{
		ID:            issued.ID,
		AgentID:       issued.AgentID,
		Version:       issued.Version,
		Rules:         wireRules(issued.Rules),
		EffectiveFrom: issued.EffectiveFrom,
	})
}

func (b rulesBody) rules() (Rules, error) {
	amounts := make([]currency.Amount, 3)
	for i, raw := range []string{b.MaxPerTx, b.MaxPerDay, b.MaxPerMonth} {
		a, err := currency.Parse(raw, b.Asset)
		if err != nil {
			return Rules{}, apperr.Invalid("caps must be decimal strings in the mandate's asset")
		}
		amounts[i] = a
	}
	return Rules{
		Asset:       b.Asset,
		MaxPerTx:    amounts[0],
		MaxPerDay:   amounts[1],
		MaxPerMonth: amounts[2],
		Categories:  b.Categories,
		PayTo:       b.PayTo,
		ExpiresAt:   b.ExpiresAt,
	}, nil
}

func wireRules(r Rules) rulesBody {
	payTo := r.PayTo
	if payTo == nil {
		payTo = []string{}
	}
	return rulesBody{
		Asset:       r.Asset,
		MaxPerTx:    r.MaxPerTx.String(),
		MaxPerDay:   r.MaxPerDay.String(),
		MaxPerMonth: r.MaxPerMonth.String(),
		Categories:  r.Categories,
		PayTo:       payTo,
		ExpiresAt:   r.ExpiresAt,
	}
}

// abort records err for server.ErrorMapper, which writes the response, and
// stops the chain.
func abort(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}
