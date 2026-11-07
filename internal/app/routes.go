package app

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/mail"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/ratelimit"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
)

// surfaces are the three ways into the API (Task 6.1). Route grouping is a
// security boundary, not decoration:
//
//	public  - no credential: sign-up, login and password reset, which are
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

// limiters cap each surface's callers (Task 6.1): the public surface per
// client address, since it has no other name for a caller; the agent surface
// per agent; the console per person.
type limiters struct {
	public, agent, console *ratelimit.Limiter
}

func newLimiters(cfg config.RateLimit, counter ratelimit.Counter, log *slog.Logger) (limiters, error) {
	var l limiters
	var err error
	if l.public, err = ratelimit.New(counter, "public", cfg.Public, time.Minute, log); err != nil {
		return limiters{}, err
	}
	if l.agent, err = ratelimit.New(counter, "agent", cfg.Agent, time.Minute, log); err != nil {
		return limiters{}, err
	}
	if l.console, err = ratelimit.New(counter, "console", cfg.Console, time.Minute, log); err != nil {
		return limiters{}, err
	}
	return l, nil
}

// registerRoutes builds the surfaces and registers every module's routes on
// the one it belongs to.
//
// Each authenticated surface limits after it authenticates, because the limit
// is per caller and the caller is not known before. Health sits outside every
// surface and every limit: a load balancer polls it from a handful of
// addresses, and throttling it would take healthy instances out of rotation.
func (a *API) registerRoutes(l limiters) {
	v1 := a.Engine.Group("/v1")
	v1.GET("/health", a.health)

	a.surfaces = surfaces{
		public:  v1.Group("", l.public.Middleware(byClientIP)),
		agent:   v1.Group("", a.accounts.AgentAuth(), l.agent.Middleware(byAgent)),
		console: v1.Group("", a.accounts.UserAuth(), l.console.Middleware(byUser)),
	}

	a.accounts.RegisterPublicRoutes(a.surfaces.public)
	a.accounts.RegisterConsoleRoutes(a.surfaces.console)
	a.mandates.RegisterConsoleRoutes(a.surfaces.console)
	a.decisions.RegisterConsoleRoutes(a.surfaces.console)
	a.summaries.RegisterConsoleRoutes(a.surfaces.console)
	a.wallets.RegisterConsoleRoutes(a.surfaces.console)
}

// byClientIP counts against the address server.New's trusted proxies let
// c.ClientIP believe, so a client cannot pick its own.
func byClientIP(c *gin.Context) (string, bool) {
	ip := c.ClientIP()
	return ip, ip != ""
}

func byAgent(c *gin.Context) (string, bool) {
	agent, ok := caller.AgentFrom(c.Request.Context())
	return string(agent.AgentID), ok
}

func byUser(c *gin.Context) (string, bool) {
	user, ok := caller.UserFrom(c.Request.Context())
	return string(user.UserID), ok
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
