// Command console does, over the API, what the account owner would do in the
// Cleargate console, which has no UI yet: sign up, enrol a second factor,
// connect an agent, approve its mandate, issue its key, and watch what it does.
//
//	go run ./console setup       # sign up once, then a fresh agent each run
//	go run ./console fund        # print the deposit address, wait for the credit
//	go run ./console status      # balance and each agent's spend against its caps
//	go run ./console decisions   # the agent's decision log, declines included
//	go run ./console freeze      # stop every agent at once
//	go run ./console unfreeze
//
// It keeps the owner's demo login, TOTP secret and session in .state/console.json.
// That is fine for a devnet demo and nothing else.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// state is everything the console remembers between runs.
type state struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret"`
	// LastStep is the TOTP step last spent. The server refuses a code from a
	// step it has already seen, so the next code must come from a later one.
	LastStep  int64  `json:"last_step"`
	Session   string `json:"session"`
	AccountID string `json:"account_id"`
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Agents    int    `json:"agents"`
}

type console struct {
	api  string
	dir  string
	st   state
	http *http.Client
}

// apiError is the API's error body. Branch on Kind and Code, never on Message.
type apiError struct {
	Status  int
	Message string `json:"error"`
	Kind    string `json:"kind"`
	Code    string `json:"code"`
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%d %s (%s): %s", e.Status, e.Kind, e.Code, e.Message)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Kind, e.Message)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./console setup|fund|status|decisions|freeze|unfreeze")
		os.Exit(2)
	}
	c := &console{
		api:  strings.TrimSuffix(envOr("CLEARGATE_API_URL", "http://localhost:8080"), "/"),
		dir:  envOr("DEMO_STATE_DIR", ".state"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
	if err := c.load(); err != nil {
		fail(err)
	}
	if os.Args[1] != "setup" && c.st.AccountID == "" {
		fail(errors.New("no demo account yet: run `go run ./console setup` first"))
	}

	var err error
	switch os.Args[1] {
	case "setup":
		err = c.setup()
	case "fund":
		err = c.fund()
	case "status":
		err = c.status()
	case "decisions":
		err = c.decisions()
	case "freeze":
		err = c.call(http.MethodPost, "/v1/accounts/"+c.st.AccountID+"/freeze", nil, nil)
		if err == nil {
			fmt.Println("Account frozen: every agent's next payment is declined account_blocked.")
		}
	case "unfreeze":
		err = c.withStepUp(func() error {
			return c.call(http.MethodPost, "/v1/accounts/"+c.st.AccountID+"/unfreeze", nil, nil)
		})
		if err == nil {
			fmt.Println("Account unfrozen: agents may spend again, each within its mandate.")
		}
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fail(err)
	}
}

// setup signs the owner up the first time, then connects a fresh agent on every
// run. A fresh agent starts with empty spend counters, so the scenario plays
// out the same way however many times it is recorded in a day.
func (c *console) setup() error {
	merchant, err := c.merchantAddress()
	if err != nil {
		return err
	}

	if c.st.AccountID == "" {
		if err := c.signUp(); err != nil {
			return err
		}
	}

	c.st.Agents++
	name := fmt.Sprintf("Research agent %d", c.st.Agents)
	var agent struct {
		ID string `json:"id"`
	}
	if err := c.call(http.MethodPost, "/v1/accounts/"+c.st.AccountID+"/agents", map[string]string{"name": name}, &agent); err != nil {
		return fmt.Errorf("connect agent: %w", err)
	}
	c.st.AgentID, c.st.AgentName = agent.ID, name
	if err := c.save(); err != nil {
		return err
	}
	say("Connected %q (%s)", name, agent.ID)

	// Setting a mandate and issuing a key both grant authority, so both sit
	// behind the 15-minute step-up window that one TOTP code opens.
	rules := mandateRules(merchant)
	var key struct {
		Key string `json:"key"`
	}
	err = c.withStepUp(func() error {
		if err := c.call(http.MethodPut, "/v1/agents/"+agent.ID+"/mandate", rules, nil); err != nil {
			return fmt.Errorf("set mandate: %w", err)
		}
		return c.call(http.MethodPost, "/v1/agents/"+agent.ID+"/credentials", map[string]string{"name": "demo laptop"}, &key)
	})
	if err != nil {
		return err
	}
	// Saved before anything is printed: the key is shown once, and only a
	// digest of it stays on the server.
	env := fmt.Sprintf("CLEARGATE_API_URL=%s\nCLEARGATE_API_KEY=%s\nMERCHANT_URL=%s\n",
		c.api, key.Key, envOr("MERCHANT_URL", "http://localhost:4021"))
	if err := os.WriteFile(filepath.Join(c.dir, "agent.env"), []byte(env), 0o600); err != nil {
		return err
	}
	say("Approved its mandate:")
	printRules(rules)
	say("Issued its API key, shown once and saved to %s", filepath.Join(c.dir, "agent.env"))

	address, err := c.depositAddress()
	if err != nil {
		return err
	}
	fmt.Printf("\nThe account's wallet (its deposit address): %s\n", address)
	fmt.Println("Fund it with devnet USDC if you have not yet: go run ./console fund")
	return nil
}

// mandateRules are the demo's spending rules, sized so the scenario meets each
// kind of refusal: a quote and a report fit, the full dataset is over the
// per-payment cap, a second report is over the daily cap, and the partner's
// route pays someone the allowlist does not name.
func mandateRules(merchant string) map[string]any {
	return map[string]any{
		"asset":         "USD",
		"max_per_tx":    "0.60",
		"max_per_day":   "1.00",
		"max_per_month": "20.00",
		"categories":    []string{"market_data", "research"},
		"pay_to":        []string{merchant},
		"expires_at":    time.Now().UTC().Add(30 * 24 * time.Hour).Format(time.RFC3339),
	}
}

func printRules(r map[string]any) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, k := range []string{"asset", "max_per_tx", "max_per_day", "max_per_month", "categories", "pay_to", "expires_at"} {
		_, _ = fmt.Fprintf(w, "      %s\t%v\n", k, r[k])
	}
	_ = w.Flush()
}

func (c *console) signUp() error {
	c.st.Email = fmt.Sprintf("owner+%d@example.com", time.Now().Unix())
	c.st.Password = randomHex(16)
	signup := map[string]string{"email": c.st.Email, "password": c.st.Password, "display_name": "Atlas Research Desk"}
	if err := c.call(http.MethodPost, "/v1/auth/signup", signup, nil); err != nil {
		return fmt.Errorf("sign up: %w", err)
	}
	if err := c.save(); err != nil {
		return err
	}
	say("Signed up %s", c.st.Email)

	token, err := c.verificationToken()
	if err != nil {
		return err
	}
	if err := c.call(http.MethodPost, "/v1/auth/verify-email", map[string]string{"token": token}, nil); err != nil {
		return fmt.Errorf("verify email: %w", err)
	}
	say("Verified the email address")

	if err := c.login(); err != nil {
		return err
	}

	var enrol struct {
		Secret string `json:"secret"`
	}
	if err := c.call(http.MethodPost, "/v1/mfa/totp", nil, &enrol); err != nil {
		return fmt.Errorf("begin TOTP: %w", err)
	}
	c.st.TOTPSecret = enrol.Secret
	code, err := c.nextCode()
	if err != nil {
		return err
	}
	if err := c.call(http.MethodPost, "/v1/mfa/totp/confirm", map[string]string{"code": code}, nil); err != nil {
		return fmt.Errorf("confirm TOTP: %w", err)
	}
	say("Enrolled TOTP as the second factor")

	var accounts struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	if err := c.call(http.MethodGet, "/v1/accounts", nil, &accounts); err != nil {
		return err
	}
	if len(accounts.Accounts) == 0 {
		return errors.New("signed up, but the person belongs to no account")
	}
	c.st.AccountID = accounts.Accounts[0].ID
	say("Created account %s", c.st.AccountID)
	return c.save()
}

// verificationToken finds the link the API's logging mailer wrote for this
// address in .state/api.log, which `./run.sh api` tees the API's output into.
// Without that log it asks for the link instead.
func (c *console) verificationToken() (string, error) {
	logFile := filepath.Join(c.dir, "api.log")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if token := tokenFromLog(logFile, c.st.Email); token != "" {
			return token, nil
		}
	}

	fmt.Printf("Paste the verification link for %s from the API's log: ", c.st.Email)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read the verification link: %w", err)
	}
	line = strings.TrimSpace(line)
	if u, err := url.Parse(line); err == nil && u.Query().Get("token") != "" {
		return u.Query().Get("token"), nil
	}
	return line, nil
}

// tokenFromLog returns the token in the last verification link logged for to.
func tokenFromLog(path, to string) string {
	raw, err := os.ReadFile(path) // #nosec G304 -- the demo's own log file
	if err != nil {
		return ""
	}
	var token string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "to="+to) {
			continue
		}
		if m := verifyLink.FindStringSubmatch(line); m != nil {
			if t, err := url.QueryUnescape(m[1]); err == nil {
				token = t
			}
		}
	}
	return token
}

// verifyLink matches the logged link, quoted or not.
var verifyLink = regexp.MustCompile(`/verify-email\?token=([^"\s]+)`)

func (c *console) login() error {
	body, _ := json.Marshal(map[string]string{"email": c.st.Email, "password": c.st.Password})
	req, err := http.NewRequest(http.MethodPost, c.api+"/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Cleargate-Console", "demo")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return decodeError(res)
	}
	// The cookie is Secure, which a cookie jar will not send back over plain
	// http://localhost, so the session is carried by hand instead.
	for _, cookie := range res.Cookies() {
		if cookie.Name == "cg_session" {
			c.st.Session = cookie.Value
		}
	}
	if c.st.Session == "" {
		return errors.New("login set no cg_session cookie")
	}
	say("Logged in")
	return c.save()
}

// withStepUp runs fn inside a fresh step-up window, opened with the next TOTP
// code, as a person would by typing the code from their authenticator app.
func (c *console) withStepUp(fn func() error) error {
	code, err := c.nextCode()
	if err != nil {
		return err
	}
	if err := c.call(http.MethodPost, "/v1/mfa/step-up", map[string]string{"code": code}, nil); err != nil {
		return fmt.Errorf("step up: %w", err)
	}
	return fn()
}

// nextCode is the TOTP code for the first step after the last one spent,
// waiting for it if need be. The server claims each step once.
func (c *console) nextCode() (string, error) {
	if c.st.TOTPSecret == "" {
		return "", errors.New("no TOTP secret saved; delete .state/console.json and run setup again")
	}
	for c.st.LastStep >= time.Now().Unix()/30 {
		wait := time.Duration(30-time.Now().Unix()%30) * time.Second
		say("Waiting %s for a fresh TOTP code", wait)
		time.Sleep(wait)
	}
	now := time.Now()
	code, err := totp.GenerateCodeCustom(c.st.TOTPSecret, now, totp.ValidateOpts{
		Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		return "", err
	}
	c.st.LastStep = now.Unix() / 30
	return code, c.save()
}

type depositAddress struct {
	Address string `json:"address"`
	Network string `json:"network"`
	Mint    string `json:"mint"`
}

func (c *console) depositAddress() (string, error) {
	var d depositAddress
	if err := c.call(http.MethodGet, "/v1/accounts/"+c.st.AccountID+"/deposit-address", nil, &d); err != nil {
		return "", fmt.Errorf("deposit address: %w", err)
	}
	return d.Address, nil
}

type summary struct {
	Balance struct {
		Amount string `json:"amount"`
	} `json:"balance"`
	Account struct {
		FrozenAt *string `json:"frozen_at"`
	} `json:"account"`
	Agents []struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Mandate *struct {
			Version   int    `json:"version"`
			MaxPerTx  string `json:"max_per_tx"`
			MaxPerDay string `json:"max_per_day"`
		} `json:"mandate"`
		SpentToday     string `json:"spent_today"`
		SpentThisMonth string `json:"spent_this_month"`
		Declined       struct {
			Count int `json:"count"`
		} `json:"declined_this_month"`
	} `json:"agents"`
}

func (c *console) summary() (summary, error) {
	var s summary
	err := c.call(http.MethodGet, "/v1/accounts/"+c.st.AccountID+"/summary", nil, &s)
	return s, err
}

// fund prints where to send devnet USDC and waits for the worker to credit it.
// The worker reads the chain at `finalized` every 30 seconds, so a deposit
// shows up about a minute after the faucet sends it.
func (c *console) fund() error {
	address, err := c.depositAddress()
	if err != nil {
		return err
	}
	start, err := c.summary()
	if err != nil {
		return err
	}
	fmt.Printf("Send devnet USDC to the account's wallet:\n\n    %s\n\n", address)
	fmt.Println("1. Open https://faucet.circle.com")
	fmt.Println("2. Pick USDC and Solana Devnet, paste the address above, and send.")
	fmt.Println("   Keep the account's total deposits under 100 USDC: past the basic tier's cap a deposit is held, not credited.")
	fmt.Printf("\nBalance now %s USD. Waiting for the credit (Ctrl-C to stop)...\n", start.Balance.Amount)

	for {
		time.Sleep(10 * time.Second)
		s, err := c.summary()
		if err != nil {
			return err
		}
		if s.Balance.Amount != start.Balance.Amount {
			fmt.Printf("Credited: balance is now %s USD.\n", s.Balance.Amount)
			return nil
		}
	}
}

func (c *console) status() error {
	s, err := c.summary()
	if err != nil {
		return err
	}
	frozen := "no"
	if s.Account.FrozenAt != nil {
		frozen = "since " + *s.Account.FrozenAt
	}
	fmt.Printf("Balance %s USD (frozen: %s)\n\n", s.Balance.Amount, frozen)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "AGENT\tSTATUS\tMANDATE\tSPENT TODAY\tSPENT THIS MONTH\tDECLINED")
	for _, a := range s.Agents {
		mandate := "none"
		if a.Mandate != nil {
			mandate = fmt.Sprintf("v%d: %s/payment, %s/day", a.Mandate.Version, a.Mandate.MaxPerTx, a.Mandate.MaxPerDay)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", a.Name, a.Status, mandate, a.SpentToday, a.SpentThisMonth, a.Declined.Count)
	}
	return w.Flush()
}

func (c *console) decisions() error {
	var page struct {
		Decisions []struct {
			Outcome    string `json:"outcome"`
			ReasonCode string `json:"reason_code"`
			LatencyMS  int    `json:"latency_ms"`
			At         string `json:"evaluated_at"`
			Payment    struct {
				Amount      string  `json:"amount"`
				Category    *string `json:"category"`
				Status      string  `json:"status"`
				ResourceURL *string `json:"resource_url"`
			} `json:"payment"`
		} `json:"decisions"`
	}
	if err := c.call(http.MethodGet, "/v1/agents/"+c.st.AgentID+"/decisions?limit=20", nil, &page); err != nil {
		return err
	}
	fmt.Printf("Decision log for %s, newest first:\n\n", c.st.AgentName)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "TIME\tOUTCOME\tREASON\tAMOUNT\tCATEGORY\tPAYMENT\tRESOURCE\tMS")
	for _, d := range page.Decisions {
		at := d.At
		if t, err := time.Parse(time.RFC3339Nano, d.At); err == nil {
			at = t.Local().Format("15:04:05")
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			at, d.Outcome, d.ReasonCode, d.Payment.Amount, deref(d.Payment.Category), d.Payment.Status, deref(d.Payment.ResourceURL), d.LatencyMS)
	}
	return w.Flush()
}

// call sends one console request. A session that has expired is renewed once
// with the saved login, as a person would sign in again.
func (c *console) call(method, path string, in, out any) error {
	err := c.send(method, path, in, out)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized && c.st.Password != "" && !strings.HasPrefix(path, "/v1/auth/") {
		if err := c.login(); err != nil {
			return err
		}
		return c.send(method, path, in, out)
	}
	return err
}

func (c *console) send(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.api+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Every console request that changes something must carry this header.
	req.Header.Set("X-Cleargate-Console", "demo")
	if c.st.Session != "" {
		// #nosec G124 -- a cookie sent, not set: its attributes are the server's.
		req.AddCookie(&http.Cookie{Name: "cg_session", Value: c.st.Session})
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w (is the API running?)", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		return decodeError(res)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func decodeError(res *http.Response) error {
	e := &apiError{Status: res.StatusCode}
	raw, _ := io.ReadAll(res.Body)
	if json.Unmarshal(raw, e) != nil || e.Kind == "" {
		e.Message = strings.TrimSpace(string(raw))
	}
	return e
}

// merchantAddress is the payee the mandate allows: the demo merchant's wallet.
func (c *console) merchantAddress() (string, error) {
	if v := os.Getenv("MERCHANT_PAY_TO"); v != "" {
		return v, nil
	}
	raw, err := os.ReadFile(filepath.Join(c.dir, "merchant.json"))
	if err != nil {
		return "", errors.New("no merchant wallet yet: run `go run ./merchant keygen` first")
	}
	var m struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Address == "" {
		return "", fmt.Errorf("%s holds no merchant address", filepath.Join(c.dir, "merchant.json"))
	}
	return m.Address, nil
}

func (c *console) load() error {
	raw, err := os.ReadFile(filepath.Join(c.dir, "console.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &c.st)
}

func (c *console) save() error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	// #nosec G117 -- the demo owner's own login, kept in a 0600 file on purpose.
	raw, err := json.MarshalIndent(c.st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.dir, "console.json"), raw, 0o600)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

func say(format string, args ...any) { fmt.Printf("  ✓ "+format+"\n", args...) }

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
