package account

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
)

// The only file in this module that knows Gin exists.

// SessionCookie is the cookie a console session travels in. Login sets it
// HttpOnly, Secure and SameSite=Strict (setSessionCookie), so script on a page
// cannot read it and another site cannot send it.
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

// RegisterPublicRoutes adds sign-up, email verification, login, logout and
// password reset (Task 2.3). They take no credential, since they are where a
// session comes from, so pass the public group.
//
// Each of them still needs ConsoleHeader. Only the console calls them, and
// without the header a page on another site could post a form that signs the
// visitor into an account the attacker controls (login CSRF), after which
// whatever the visitor funds or approves lands there.
func (m *Module) RegisterPublicRoutes(rg *gin.RouterGroup) {
	auth := rg.Group("/auth", requireConsoleHeader)
	auth.POST("/signup", m.signup)
	auth.POST("/verify-email", m.verifyEmail)
	auth.POST("/login", m.login)
	auth.POST("/logout", m.logout)
	auth.POST("/password/reset-request", m.requestPasswordReset)
	auth.POST("/password/reset", m.resetPassword)
}

// maxAuthBody bounds what an unauthenticated caller can make the server read.
// The largest honest body is a password at crypto.MaxPasswordLength plus an
// address and a name.
const maxAuthBody = 16 << 10

// maxUserAgent bounds the User-Agent stored on the session and the audit
// trail. It is a description of the client, not evidence, so cutting it is
// harmless.
const maxUserAgent = 512

var errBadBody = apperr.Invalid("the body must be JSON with the fields this route takes")

type credentialsBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type signupBody struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type tokenBody struct {
	Token string `json:"token"`
}

type resetBody struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

type emailBody struct {
	Email string `json:"email"`
}

type sessionResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// signup always answers 202 with no body, registered address or not, so the
// answer cannot be used to test a list of addresses (Task 2.3).
func (m *Module) signup(c *gin.Context) {
	var body signupBody
	if !bind(c, &body) {
		return
	}
	ip, ua := client(c)
	if err := m.svc.Signup(c.Request.Context(), SignupParams{
		Email: body.Email, Password: body.Password, DisplayName: body.DisplayName,
		IP: ip, UserAgent: ua,
	}); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusAccepted)
}

func (m *Module) verifyEmail(c *gin.Context) {
	var body tokenBody
	if !bind(c, &body) {
		return
	}
	ip, ua := client(c)
	if err := m.svc.VerifyEmail(c.Request.Context(), body.Token, ip, ua); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// login sets the session cookie. The token goes only there, never in the
// body, so no script on the console's pages can read it.
func (m *Module) login(c *gin.Context) {
	var body credentialsBody
	if !bind(c, &body) {
		return
	}
	ip, ua := client(c)
	token, session, err := m.svc.Login(c.Request.Context(), LoginParams{
		Email: body.Email, Password: body.Password, IP: ip, UserAgent: ua,
	})
	if err != nil {
		abort(c, err)
		return
	}
	setSessionCookie(c, token.Plaintext(), int(session.ExpiresAt.Sub(session.IssuedAt).Seconds()))
	c.JSON(http.StatusOK, sessionResponse{ExpiresAt: session.ExpiresAt})
}

// logout ends the session in the cookie, if there is one, and clears the
// cookie either way: a client asking to be signed out is signed out.
func (m *Module) logout(c *gin.Context) {
	if token, err := c.Cookie(SessionCookie); err == nil && token != "" {
		ip, ua := client(c)
		if err := m.svc.Logout(c.Request.Context(), token, ip, ua); err != nil {
			abort(c, err)
			return
		}
	}
	setSessionCookie(c, "", -1)
	c.Status(http.StatusNoContent)
}

// requestPasswordReset answers 202 whether or not the address exists.
func (m *Module) requestPasswordReset(c *gin.Context) {
	var body emailBody
	if !bind(c, &body) {
		return
	}
	ip, ua := client(c)
	if err := m.svc.RequestPasswordReset(c.Request.Context(), body.Email, ip, ua); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusAccepted)
}

func (m *Module) resetPassword(c *gin.Context) {
	var body resetBody
	if !bind(c, &body) {
		return
	}
	ip, ua := client(c)
	if err := m.svc.ResetPassword(c.Request.Context(), body.Token, body.Password, ip, ua); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// setSessionCookie writes the session cookie, or with maxAge -1 deletes it.
// HttpOnly keeps it from script, Secure from plain HTTP, and SameSite=Strict
// from requests another site starts. It is host-only, with no Domain, so no
// sibling subdomain receives it.
func setSessionCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// bind decodes a JSON body of at most maxAuthBody bytes into dst.
func bind(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAuthBody)
	if err := c.ShouldBindJSON(dst); err != nil {
		abort(c, errBadBody)
		return false
	}
	return true
}

// client is who is asking, for the throttle and the audit trail. The address
// is the one server.New's trusted proxies allow c.ClientIP to believe. The
// User-Agent is made storable: Postgres refuses invalid UTF-8 and NUL, and a
// session must not fail to start over a header.
func client(c *gin.Context) (ip, userAgent string) {
	ua := strings.ReplaceAll(strings.ToValidUTF8(c.Request.UserAgent(), ""), "\x00", "")
	if len(ua) > maxUserAgent {
		ua = strings.ToValidUTF8(ua[:maxUserAgent], "")
	}
	return c.ClientIP(), ua
}

func requireConsoleHeader(c *gin.Context) {
	if c.GetHeader(ConsoleHeader) == "" {
		abort(c, errConsoleHeaderMissing)
		return
	}
	c.Next()
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
