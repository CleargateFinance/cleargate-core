package settlement

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// The only file in this module that knows Gin exists.
// Handlers decode, delegate to Service, and encode. No business logic here.

// RegisterConsoleRoutes adds the deposit address. Pass it the console group:
// funding an account is a person's act, and an agent has no business reading
// where its owner's money sits (§9.3).
func (m *Module) RegisterConsoleRoutes(rg *gin.RouterGroup) {
	rg.GET("/accounts/:id/deposit-address", m.depositAddress)
}

// errNotPermitted is the account module's answer to a non-member, word for
// word, so an account id that is not a UUID, one that names nothing and one
// that is somebody else's cannot be told apart.
var errNotPermitted = apperr.Forbidden("your role does not permit this change")

type depositAddressResponse struct {
	Address string `json:"address"`
	Network string `json:"network"`
	Token   string `json:"token"`
	Mint    string `json:"mint"`
}

func (m *Module) depositAddress(c *gin.Context) {
	user, ok := caller.UserFrom(c.Request.Context())
	if !ok {
		// Registered outside the console group. A wiring defect, and the
		// route must not create a wallet for nobody.
		abort(c, apperr.Internal("settlement: route reached without a console session", nil))
		return
	}
	accountID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		abort(c, errNotPermitted)
		return
	}

	deposit, err := m.svc.DepositAddress(c.Request.Context(), user.UserID, id.AccountID(accountID.String()))
	if err != nil {
		abort(c, err)
		return
	}
	c.JSON(http.StatusOK, depositAddressResponse{
		Address: deposit.Address, Network: deposit.Token.Network,
		Token: deposit.Token.Symbol, Mint: deposit.Token.Mint,
	})
}

func abort(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}
