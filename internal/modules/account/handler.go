package account

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
)

// The only file in this module that knows Gin exists.

// SessionCookie is the cookie a console session travels in. Login sets it
// (Task 6.2) HttpOnly, Secure and SameSite=Strict, so script on a page cannot
// read it and another site cannot send it.
const SessionCookie = "cg_session"

// ConsoleHeader must accompany every console request that changes something.
// A browser sends a custom header cross-origin only after a CORS preflight
// the API refuses for other origins, so a forged form or link on another site
// cannot carry it (OWASP's custom request header defence against CSRF). It is
// a second line behind SameSite, which a same-site subdomain gets past.
const ConsoleHeader = "X-Cleargate-Console"

var (
	errAgentKeyRequired     = apperr.Unauthorized("this route needs an agent API key in the Authorization header")
	errSessionRequired      = apperr.Unauthorized("this route needs a console session")
	errConsoleHeaderMissing = apperr.Forbidden("console requests that change something must send the " + ConsoleHeader + " header")
)

// AgentAuth authenticates the agent surface. It reads only a bearer API key
// and ignores any session cookie, so a console session reaches no agent route
// (Phase 2: the surfaces share no credential).
func (m *Module) AgentAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || key == "" {
			abort(c, errAgentKeyRequired)
			return
		}
		identity, err := m.svc.AuthenticateAgent(c.Request.Context(), key)
		if err != nil {
			abort(c, err)
			return
		}
		c.Request = c.Request.WithContext(caller.WithAgent(c.Request.Context(), caller.Agent{
			AgentID:      identity.AgentID,
			AccountID:    identity.AccountID,
			CredentialID: identity.CredentialID,
		}))
		c.Next()
	}
}

// UserAuth authenticates the console surface. It reads only the session
// cookie and ignores the Authorization header, so an agent's API key reaches
// no console route, and in particular none that changes a mandate (§6.1).
//
// A request that changes something must also carry ConsoleHeader.
func (m *Module) UserAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(SessionCookie)
		if err != nil || token == "" {
			abort(c, errSessionRequired)
			return
		}
		session, err := m.svc.ValidateSession(c.Request.Context(), token)
		if err != nil {
			abort(c, err)
			return
		}
		if !safeMethod(c.Request.Method) && c.GetHeader(ConsoleHeader) == "" {
			abort(c, errConsoleHeaderMissing)
			return
		}
		c.Request = c.Request.WithContext(caller.WithUser(c.Request.Context(), caller.User{
			UserID:         session.UserID,
			MFASatisfiedAt: session.MFASatisfiedAt,
		}))
		c.Next()
	}
}

// abort records err for server.ErrorMapper, which writes the response, and
// stops the chain. c.Error returns the error it was given, wrapped, so there
// is nothing to check.
func abort(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}
