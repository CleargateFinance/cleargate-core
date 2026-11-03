package app

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/mail"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
)

// surfaces are the three ways into the API (Task 6.1). Route grouping is a
// security boundary, not decoration:
//
//	public  - no credential: health, and later sign-up and login, which are
//	          where sessions come from
//	agent   - an agent's API key; may REQUEST payments
//	console - a person's session; may CREATE and SIGN mandates
//
// Each authenticated surface reads only its own credential, so a request
// carrying the other kind is simply unauthenticated there. Mandate-mutating
// routes are registered only on the console surface: an agent key cannot
// reach them even if a handler forgets a check, because on the agent surface
// they do not exist. That is §6.1 made structural: a compromised agent must
// not be able to widen its own authority.
type surfaces struct {
	public  *gin.RouterGroup
	agent   *gin.RouterGroup
	console *gin.RouterGroup
}

// registerRoutes builds the surfaces and registers every module's routes on
// the one it belongs to.
func (a *API) registerRoutes() {
	v1 := a.Engine.Group("/v1")
	a.surfaces = surfaces{
		public:  v1,
		agent:   v1.Group("", a.accounts.AgentAuth()),
		console: v1.Group("", a.accounts.UserAuth()),
	}

	a.surfaces.public.GET("/health", a.health)
	a.mandates.RegisterConsoleRoutes(a.surfaces.console)
}

// errNoMailTransport stops the API starting where it cannot send email
// safely. There is no production transport yet (the provider is still to be
// chosen), and the only one that exists hands out working credentials.
var errNoMailTransport = errors.New("app: MAIL_TRANSPORT")

// newMailer picks the mail transport the config names. Nothing is chosen by
// default, because the only transport that needs no configuration writes live
// sign-in links to the log.
func newMailer(cfg *config.Config, log *slog.Logger) (account.Mailer, error) {
	switch cfg.Mail.Transport {
	case "log":
		if cfg.Server.Mode != "debug" {
			return nil, fmt.Errorf("%w: log writes working credentials to the log and is refused outside SERVER_MODE=debug", errNoMailTransport)
		}
		return mail.NewLogging(log, cfg.Mail.ConsoleURL), nil
	case "":
		return nil, fmt.Errorf("%w is required; use log in development", errNoMailTransport)
	default:
		return nil, fmt.Errorf("%w: unknown transport %q", errNoMailTransport, cfg.Mail.Transport)
	}
}
