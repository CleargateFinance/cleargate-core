package payment

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// The only file in this module that knows Gin exists.
// Handlers decode, delegate to Service, and encode. No business logic here.

// maxAuthorizeBody bounds a request an agent's key can send. A requirement with
// many options is a few kilobytes, and the signals have bounds of their own.
const maxAuthorizeBody = 64 << 10

// RegisterAgentRoutes adds the payment route. Pass it the agent group and
// nothing else: paying is what an agent's key is for (Task 6.1).
func (m *Module) RegisterAgentRoutes(rg *gin.RouterGroup) {
	rg.POST("/payments/authorize", m.authorize)
}

type authorizeRequest struct {
	// Requirement is the merchant's PAYMENT-REQUIRED, unchanged.
	Requirement json.RawMessage `json:"requirement"`
	Category    string          `json:"category"`

	ResourceURL                string          `json:"resource_url"`
	URLSource                  authz.URLSource `json:"url_source"`
	Purpose                    string          `json:"purpose"`
	Task                       string          `json:"task"`
	SessionID                  string          `json:"session_id"`
	SessionNovelCounterparties *int            `json:"session_novel_counterparties"`
}

type authorizeResponse struct {
	PaymentID  id.PaymentID     `json:"payment_id"`
	Status     Status           `json:"status"`
	Outcome    authz.Outcome    `json:"outcome"`
	ReasonCode authz.ReasonCode `json:"reason_code"`
	Reason     string           `json:"reason"`
	DecidedAt  time.Time        `json:"decided_at"`
	Replayed   bool             `json:"replayed"`

	// PaymentSignature is the value of the PAYMENT-SIGNATURE header to retry the
	// merchant with. Null unless the agent may pay.
	PaymentSignature *string `json:"payment_signature"`
}

// authorize decides a payment and, if it is approved, signs it. A decline is a
// 200 too: the request worked, and the answer is no (Task 4.1).
func (m *Module) authorize(c *gin.Context) {
	agent, ok := caller.AgentFrom(c.Request.Context())
	if !ok {
		// Registered outside the agent group. A wiring defect, and no payment
		// may run without the credential that names its account.
		abort(c, apperr.Internal("payment: route reached without an agent credential", nil))
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuthorizeBody)
	var body authorizeRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		abort(c, apperr.Invalid("the body must be a payment requirement and its signals as JSON"))
		return
	}

	paid, err := m.svc.Pay(c.Request.Context(), PayRequest{
		AccountID:      agent.AccountID,
		AgentID:        agent.AgentID,
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
		Requirement:    body.Requirement,
		Category:       body.Category,
		Signals: authz.Signals{
			ResourceURL:                body.ResourceURL,
			URLSource:                  body.URLSource,
			Purpose:                    body.Purpose,
			Task:                       body.Task,
			SessionID:                  body.SessionID,
			SessionNovelCounterparties: body.SessionNovelCounterparties,
		},
	})
	if err != nil {
		abort(c, err)
		return
	}

	res := authorizeResponse{
		PaymentID:  paid.PaymentID,
		Status:     paid.Status,
		Outcome:    paid.Outcome,
		ReasonCode: paid.ReasonCode,
		Reason:     paid.Reason,
		DecidedAt:  paid.DecidedAt,
		Replayed:   paid.Replayed,
	}
	if paid.PaymentSignature != "" {
		res.PaymentSignature = &paid.PaymentSignature
	}
	// A signed transaction is a bearer instrument until it lands or expires.
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, res)
}

func abort(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}
