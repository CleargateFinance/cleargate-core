package account

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/caller"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
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

// RegisterConsoleRoutes adds the second-factor routes (Task 2.4), and the
// routes that connect agents, issue their keys and freeze the account
// (Task 6.2). Pass the console group: every one of them acts for a signed-in
// person, and an agent's key must reach none of them.
func (m *Module) RegisterConsoleRoutes(rg *gin.RouterGroup) {
	mfa := rg.Group("/mfa")
	mfa.GET("", m.mfaStatus)
	mfa.POST("/totp", m.beginTOTP)
	mfa.POST("/totp/confirm", m.confirmTOTP)
	mfa.POST("/step-up", m.stepUp)

	rg.GET("/accounts", m.listAccounts)
	accounts := rg.Group("/accounts/:id")
	accounts.GET("/agents", m.listAgents)
	accounts.POST("/agents", m.connectAgent)
	accounts.POST("/freeze", m.freeze)
	accounts.POST("/unfreeze", m.unfreeze)

	agents := rg.Group("/agents/:id")
	agents.POST("/revoke", m.revokeAgent)
	agents.GET("/credentials", m.listCredentials)
	agents.POST("/credentials", m.issueCredential)
	agents.POST("/credentials/:cid/revoke", m.revokeCredential)
}

var (
	errNoConsoleSession = apperr.Internal("account: route reached without a console session", nil)
	errOneCode          = apperr.Invalid("send exactly one of code or recovery_code")
)

type mfaStatusResponse struct {
	TOTP bool `json:"totp"`
}

type totpEnrolmentResponse struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

type recoveryCodesResponse struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

type stepUpBody struct {
	Code         string `json:"code"`
	RecoveryCode string `json:"recovery_code"`
}

type stepUpResponse struct {
	MFASatisfiedAt *time.Time `json:"mfa_satisfied_at"`
}

// mfaStatus tells the console whether to offer enrolment or ask for a code,
// without it having to attempt a gated action first.
func (m *Module) mfaStatus(c *gin.Context) {
	user, ok := consoleUser(c)
	if !ok {
		return
	}
	enrolled, err := m.svc.MFAEnrolled(c.Request.Context(), Session{UserID: user.UserID})
	if err != nil {
		abort(c, err)
		return
	}
	c.JSON(http.StatusOK, mfaStatusResponse{TOTP: enrolled})
}

// beginTOTP returns the shared secret once, for the console to render as a QR
// code. It is not stored in the clear anywhere, so the response must not be
// either.
func (m *Module) beginTOTP(c *gin.Context) {
	user, ok := consoleUser(c)
	if !ok {
		return
	}
	enrolment, err := m.svc.BeginTOTPEnrolment(c.Request.Context(), user.UserID)
	if err != nil {
		abort(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, totpEnrolmentResponse{Secret: enrolment.Secret, URI: enrolment.URI})
}

// confirmTOTP completes enrolment and returns the recovery codes, the only
// time they exist in the clear.
func (m *Module) confirmTOTP(c *gin.Context) {
	user, ok := consoleUser(c)
	if !ok {
		return
	}
	var body stepUpBody
	if !bind(c, &body) {
		return
	}
	ip, ua := client(c)
	codes, err := m.svc.ConfirmTOTPEnrolment(c.Request.Context(), user.UserID, body.Code, ip, ua)
	if err != nil {
		abort(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, recoveryCodesResponse{RecoveryCodes: codes})
}

// stepUp starts the session's step-up window with an authenticator code or,
// for someone whose phone is gone, one recovery code.
func (m *Module) stepUp(c *gin.Context) {
	if _, ok := consoleUser(c); !ok {
		return
	}
	var body stepUpBody
	if !bind(c, &body) {
		return
	}
	if (body.Code == "") == (body.RecoveryCode == "") {
		abort(c, errOneCode)
		return
	}

	// The service marks the session the cookie names, which UserAuth has
	// already validated.
	token, _ := c.Cookie(SessionCookie)
	ip, ua := client(c)
	var session Session
	var err error
	if body.Code != "" {
		session, err = m.svc.SatisfyTOTP(c.Request.Context(), token, body.Code, ip, ua)
	} else {
		session, err = m.svc.SatisfyRecoveryCode(c.Request.Context(), token, body.RecoveryCode, ip, ua)
	}
	if err != nil {
		abort(c, err)
		return
	}
	c.JSON(http.StatusOK, stepUpResponse{MFASatisfiedAt: session.MFASatisfiedAt})
}

type accountResponse struct {
	ID       id.AccountID   `json:"id"`
	Name     string         `json:"name"`
	Role     PermissionRole `json:"role"`
	FrozenAt *time.Time     `json:"frozen_at"`
}

type agentResponse struct {
	ID        id.AgentID   `json:"id"`
	AccountID id.AccountID `json:"account_id"`
	Name      string       `json:"name"`
	Status    AgentStatus  `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	RevokedAt *time.Time   `json:"revoked_at"`
}

type credentialResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

type issuedCredentialResponse struct {
	// Key is the whole credential, shown this once. Only its digest is kept.
	Key        string             `json:"key"`
	Credential credentialResponse `json:"credential"`
}

type nameBody struct {
	Name string `json:"name"`
}

// listAccounts is where the console starts: the accounts this person belongs
// to and their role in each.
func (m *Module) listAccounts(c *gin.Context) {
	session, ok := consoleSession(c)
	if !ok {
		return
	}
	memberships, err := m.svc.Accounts(c.Request.Context(), session)
	if err != nil {
		abort(c, err)
		return
	}
	out := make([]accountResponse, 0, len(memberships))
	for _, a := range memberships {
		out = append(out, accountResponse{ID: a.AccountID, Name: a.Name, Role: a.Role, FrozenAt: a.FrozenAt})
	}
	c.JSON(http.StatusOK, gin.H{"accounts": out})
}

func (m *Module) listAgents(c *gin.Context) {
	session, accountID, ok := accountRequest(c)
	if !ok {
		return
	}
	agents, err := m.svc.Agents(c.Request.Context(), session, accountID)
	if err != nil {
		abort(c, err)
		return
	}
	out := make([]agentResponse, 0, len(agents))
	for _, a := range agents {
		out = append(out, wireAgent(a))
	}
	c.JSON(http.StatusOK, gin.H{"agents": out})
}

// connectAgent registers an agent. It grants nothing until a key is issued
// and a mandate set, which is why it needs no step-up.
func (m *Module) connectAgent(c *gin.Context) {
	session, accountID, ok := accountRequest(c)
	if !ok {
		return
	}
	var body nameBody
	if !bind(c, &body) {
		return
	}
	agent, err := m.svc.ConnectAgent(c.Request.Context(), session, accountID, body.Name)
	if err != nil {
		abort(c, err)
		return
	}
	c.JSON(http.StatusCreated, wireAgent(agent))
}

func (m *Module) freeze(c *gin.Context) {
	session, accountID, ok := accountRequest(c)
	if !ok {
		return
	}
	if err := m.svc.FreezeAccount(c.Request.Context(), session, accountID); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (m *Module) unfreeze(c *gin.Context) {
	session, accountID, ok := accountRequest(c)
	if !ok {
		return
	}
	if err := m.svc.UnfreezeAccount(c.Request.Context(), session, accountID); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (m *Module) revokeAgent(c *gin.Context) {
	session, agentID, ok := agentRequest(c)
	if !ok {
		return
	}
	if err := m.svc.RevokeAgent(c.Request.Context(), session, agentID); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (m *Module) listCredentials(c *gin.Context) {
	session, agentID, ok := agentRequest(c)
	if !ok {
		return
	}
	credentials, err := m.svc.ListCredentials(c.Request.Context(), session, agentID)
	if err != nil {
		abort(c, err)
		return
	}
	out := make([]credentialResponse, 0, len(credentials))
	for _, cr := range credentials {
		out = append(out, wireCredential(cr))
	}
	c.JSON(http.StatusOK, gin.H{"credentials": out})
}

// issueCredential returns the key once. Like the TOTP secret, it exists in
// the clear nowhere else, so no cache may keep the response.
func (m *Module) issueCredential(c *gin.Context) {
	session, agentID, ok := agentRequest(c)
	if !ok {
		return
	}
	var body nameBody
	if !bind(c, &body) {
		return
	}
	key, credential, err := m.svc.IssueCredential(c.Request.Context(), session, agentID, body.Name)
	if err != nil {
		abort(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, issuedCredentialResponse{Key: key.Plaintext(), Credential: wireCredential(credential)})
}

func (m *Module) revokeCredential(c *gin.Context) {
	session, agentID, ok := agentRequest(c)
	if !ok {
		return
	}
	credentialID, ok := pathUUID(c, "cid", ErrCredentialNotFound)
	if !ok {
		return
	}
	if err := m.svc.RevokeCredential(c.Request.Context(), session, agentID, credentialID); err != nil {
		abort(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func wireAgent(a Agent) agentResponse {
	return agentResponse{
		ID: a.ID, AccountID: a.AccountID, Name: a.Name, Status: a.Status,
		CreatedAt: a.CreatedAt, RevokedAt: a.RevokedAt,
	}
}

func wireCredential(cr AgentCredential) credentialResponse {
	return credentialResponse{
		ID: cr.ID, Name: cr.Name, Prefix: cr.Prefix,
		CreatedAt: cr.CreatedAt, RevokedAt: cr.RevokedAt, LastUsedAt: cr.LastUsedAt,
	}
}

// accountRequest is the person and the account a /accounts/:id route names.
// An id that is not a UUID gets ErrNotPermitted, the answer an account the
// person does not belong to gets (MembershipRole), so the two cannot be told
// apart.
func accountRequest(c *gin.Context) (Session, id.AccountID, bool) {
	session, ok := consoleSession(c)
	if !ok {
		return Session{}, "", false
	}
	raw, ok := pathUUID(c, "id", ErrNotPermitted)
	return session, id.AccountID(raw), ok
}

// agentRequest is the person and the agent an /agents/:id route names. An id
// that is not a UUID is an agent that does not exist.
func agentRequest(c *gin.Context) (Session, id.AgentID, bool) {
	session, ok := consoleSession(c)
	if !ok {
		return Session{}, "", false
	}
	raw, ok := pathUUID(c, "id", ErrAgentNotFound)
	return session, id.AgentID(raw), ok
}

// pathUUID reads a path parameter that must be a UUID, answering notFound
// otherwise. Postgres would refuse the cast with an error that reads as a
// fault, when it is only an identifier that names nothing.
func pathUUID(c *gin.Context, name string, notFound error) (string, bool) {
	parsed, err := uuid.Parse(c.Param(name))
	if err != nil {
		abort(c, notFound)
		return "", false
	}
	return parsed.String(), true
}

// consoleSession is the signed-in person as the service's Session, carrying
// what the service's checks read: who they are and when they last stepped up.
func consoleSession(c *gin.Context) (Session, bool) {
	user, ok := consoleUser(c)
	return Session{UserID: user.UserID, MFASatisfiedAt: user.MFASatisfiedAt}, ok
}

// consoleUser is the person UserAuth signed in. Its absence means the route
// was registered outside the console group, and the request is refused.
func consoleUser(c *gin.Context) (caller.User, bool) {
	user, ok := caller.UserFrom(c.Request.Context())
	if !ok {
		abort(c, errNoConsoleSession)
	}
	return user, ok
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
