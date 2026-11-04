package decisionlog

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// The only file in this module that knows Gin exists.
// Handlers decode, delegate to Log, and encode. No business logic here.

// RegisterConsoleRoutes adds the decision log. Pass it the console group: the
// log is a person's view of their agents, and an agent has no business reading
// which of its payments were declined and why (§6.1).
func (m *Module) RegisterConsoleRoutes(rg *gin.RouterGroup) {
	rg.GET("/agents/:id/decisions", m.agentDecisions)
}

type decisionsResponse struct {
	Decisions []decisionJSON `json:"decisions"`

	// NextCursor is passed back as ?cursor= for the page after this one, and
	// is null on the last page.
	NextCursor *string `json:"next_cursor"`
}

type decisionJSON struct {
	ID         id.DecisionID `json:"id"`
	Outcome    string        `json:"outcome"`
	ReasonCode string        `json:"reason_code"`
	Reason     string        `json:"reason"`

	// Mandate is null for a decline made before there was one to check.
	Mandate *mandateJSON `json:"mandate"`
	Checks  []checkJSON  `json:"checks"`
	Payment paymentJSON  `json:"payment"`

	EvaluatedAt time.Time `json:"evaluated_at"`
	LatencyMS   int64     `json:"latency_ms"`
}

type mandateJSON struct {
	ID      id.MandateID `json:"id"`
	Version int          `json:"version"`

	// Digest is the SHA-256 of the mandate's approved bytes, in hex, so a
	// dispute can show which exact rules applied (data-architecture.md §4.1).
	Digest string `json:"digest"`
}

type checkJSON struct {
	Stage  string `json:"stage"`
	Result string `json:"result"`
}

// paymentJSON is what the decision was about. The amount is a decimal string,
// never a JSON number, as everywhere money crosses the wire. Fields the agent
// did not send are null.
type paymentJSON struct {
	ID          id.PaymentID   `json:"id"`
	Amount      string         `json:"amount"`
	Asset       currency.Asset `json:"asset"`
	Category    *string        `json:"category"`
	PayTo       *string        `json:"pay_to"`
	ResourceURL *string        `json:"resource_url"`
	Status      string         `json:"status"`
}

// agentDecisions serves one page of an agent's log. The query is
// ?outcome=approved|declined, ?limit= up to MaxPageSize, and ?cursor= from
// the previous page.
func (m *Module) agentDecisions(c *gin.Context) {
	user, ok := caller.UserFrom(c.Request.Context())
	if !ok {
		// Registered outside the console group. A wiring defect, and the log
		// must not be served to nobody in particular.
		abort(c, apperr.Internal("decisionlog: route reached without a console session", nil))
		return
	}

	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		abort(c, apperr.NotFound("agent not found"))
		return
	}

	q := Query{Outcome: c.Query("outcome")}
	if raw := c.Query("limit"); raw != "" {
		if q.Limit, err = strconv.Atoi(raw); err != nil || q.Limit == 0 {
			abort(c, apperr.Invalid("limit must be a whole number from 1"))
			return
		}
	}
	if raw := c.Query("cursor"); raw != "" {
		after, err := ParseCursor(raw)
		if err != nil {
			abort(c, err)
			return
		}
		q.After = &after
	}

	page, err := m.log.AgentDecisions(c.Request.Context(), Actor{UserID: user.UserID},
		id.AgentID(agentID.String()), q)
	if err != nil {
		abort(c, err)
		return
	}

	out := decisionsResponse{Decisions: make([]decisionJSON, len(page.Entries))}
	for i, e := range page.Entries {
		out.Decisions[i] = wireEntry(e)
	}
	if page.Next != nil {
		next := page.Next.String()
		out.NextCursor = &next
	}
	c.JSON(http.StatusOK, out)
}

func wireEntry(e Entry) decisionJSON {
	d := decisionJSON{
		ID:          e.ID,
		Outcome:     e.Outcome,
		ReasonCode:  e.ReasonCode,
		Reason:      e.Reason,
		Checks:      make([]checkJSON, len(e.Checks)),
		EvaluatedAt: e.EvaluatedAt,
		LatencyMS:   e.Latency.Milliseconds(),
		Payment: paymentJSON{
			ID:          e.PaymentID,
			Amount:      e.Payment.Amount.String(),
			Asset:       e.Payment.Amount.Asset(),
			Category:    orNull(e.Payment.Category),
			PayTo:       orNull(e.Payment.PayTo),
			ResourceURL: orNull(e.Payment.ResourceURL),
			Status:      e.Payment.Status,
		},
	}
	if m := e.Mandate; m != nil {
		d.Mandate = &mandateJSON{ID: m.ID, Version: m.Version, Digest: hex.EncodeToString(m.Digest)}
	}
	for i, ch := range e.Checks {
		d.Checks[i] = checkJSON(ch)
	}
	return d
}

// orNull shows a field the agent did not send as null, which is how the
// payment stored it, rather than as an empty string it never sent.
func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// abort records err for server.ErrorMapper, which writes the response, and
// stops the chain.
func abort(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}
