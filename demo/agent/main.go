// Command agent is the demo's buyer: a research agent with a task, a Cleargate
// API key, and no wallet of its own.
//
// For each thing it wants to buy it does what any x402 client does, with one
// step added. On the merchant's 402 it sends the payment requirement to
// Cleargate instead of signing it. Cleargate checks it against the mandate the
// owner approved and, if it fits, has the account's wallet sign the payment and
// returns it as the PAYMENT-SIGNATURE to retry with. If it does not fit, the
// agent gets the reason and pays nothing: it never had a key that could.
//
//	go run ./agent             # the scripted scenario
//	go run ./agent <path> <category>   # one purchase, e.g. /v1/quotes/sol market_data
package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const task = "Brief me on semiconductors: NVDA's price and an analyst view of the sector."

// purchase is one thing the agent decides to buy, and why.
type purchase struct {
	path     string
	category string
	purpose  string
}

// scenario is the demo's script. Under the demo mandate (0.60 per payment,
// 1.00 a day, market_data and research only, the merchant's wallet only):
//
//  1. a 0.01 quote and 2. a 0.50 report are approved and paid;
//  3. the 5.00 dataset is over the per-payment cap;
//  4. the partner's 0.05 feed pays a wallet the mandate does not list;
//  5. a second 0.50 report would take the day to 1.01, over the daily cap.
var scenario = []purchase{
	{"/v1/quotes/NVDA", "market_data", "Current NVDA price for the brief"},
	{"/v1/reports/semiconductors", "research", "Analyst view of the sector"},
	{"/v1/datasets/full", "market_data", "Full price history, to back-test the view"},
	{"/v1/partner/signals", "market_data", "Second opinion from a partner feed"},
	{"/v1/reports/energy", "research", "Energy costs drive fab margins"},
}

type agent struct {
	api      string
	key      string
	merchant string
	session  string
	http     *http.Client
}

func main() {
	env := loadEnv(filepath.Join(envOr("DEMO_STATE_DIR", ".state"), "agent.env"))
	a := &agent{
		api:      strings.TrimSuffix(env("CLEARGATE_API_URL", "http://localhost:8080"), "/"),
		key:      env("CLEARGATE_API_KEY", ""),
		merchant: strings.TrimSuffix(env("MERCHANT_URL", "http://localhost:4021"), "/"),
		session:  uuid.NewString(),
		// Generous, because the merchant answers a paid request only after the
		// facilitator has seen the transaction land.
		http: &http.Client{Timeout: 2 * time.Minute},
	}
	if a.key == "" {
		fmt.Fprintln(os.Stderr, "no CLEARGATE_API_KEY: run `go run ./console setup` first")
		os.Exit(1)
	}

	plan := scenario
	if len(os.Args) == 3 {
		plan = []purchase{{os.Args[1], os.Args[2], "Bought by hand"}}
	}

	fmt.Printf("Task: %s\n", task)
	for i, p := range plan {
		fmt.Printf("\n[%d/%d] %s\n", i+1, len(plan), p.purpose)
		if err := a.buy(p); err != nil {
			fmt.Printf("  ✗ %v\n", err)
		}
	}
}

// buy fetches one resource, paying through Cleargate if the merchant asks.
func (a *agent) buy(p purchase) error {
	url := a.merchant + p.path
	fmt.Printf("  → GET %s\n", url)
	res, err := a.get(url, "")
	if err != nil {
		return err
	}
	if res.status == http.StatusOK {
		fmt.Printf("  ← 200, free: %s\n", res.body)
		return nil
	}
	if res.status != http.StatusPaymentRequired {
		return fmt.Errorf("merchant answered %d: %s", res.status, res.body)
	}

	requirement, err := decodeHeader(res.header.Get("PAYMENT-REQUIRED"))
	if err != nil {
		return fmt.Errorf("unreadable PAYMENT-REQUIRED: %w", err)
	}
	price, payTo := describe(requirement)
	fmt.Printf("  ← 402 Payment Required: %s USDC to %s\n", price, short(payTo))

	decision, err := a.authorize(requirement, p, url)
	if err != nil {
		return err
	}
	if decision.Outcome != "approved" {
		// A decline is final for this purchase. The agent has no other way to
		// pay, and the API contract says it must not look for one.
		fmt.Printf("  ✗ Cleargate declined: %s (%s)\n", decision.ReasonCode, decision.Reason)
		return nil
	}
	fmt.Printf("  ✓ Cleargate approved (%s), payment %s signed by the account's wallet\n", decision.ReasonCode, decision.PaymentID)

	fmt.Println("  → GET again with PAYMENT-SIGNATURE; the merchant's facilitator submits it to Solana")
	res, err = a.get(url, *decision.PaymentSignature)
	if err != nil {
		return err
	}
	if res.status != http.StatusOK {
		return fmt.Errorf("merchant refused the payment (%d): %s", res.status, res.body)
	}
	if settled, err := decodeHeader(res.header.Get("PAYMENT-RESPONSE")); err == nil {
		var s struct {
			Transaction string `json:"transaction"`
		}
		if json.Unmarshal(settled, &s) == nil && s.Transaction != "" {
			fmt.Printf("  ✓ Landed: https://explorer.solana.com/tx/%s?cluster=devnet\n", s.Transaction)
		}
	}
	fmt.Printf("  ← 200: %s\n", res.body)
	return nil
}

type decision struct {
	PaymentID        string  `json:"payment_id"`
	Outcome          string  `json:"outcome"`
	ReasonCode       string  `json:"reason_code"`
	Reason           string  `json:"reason"`
	PaymentSignature *string `json:"payment_signature"`
}

// authorize asks Cleargate to decide the requirement and, if approved, sign it.
func (a *agent) authorize(requirement json.RawMessage, p purchase, url string) (decision, error) {
	body, err := json.Marshal(map[string]any{
		// The merchant's requirement goes unchanged. Cleargate reads the amount
		// and payee from it, so the agent cannot misstate either.
		"requirement":  requirement,
		"category":     p.category,
		"purpose":      p.purpose,
		"task":         task,
		"resource_url": url,
		"url_source":   "task",
		"session_id":   a.session,
	})
	if err != nil {
		return decision{}, err
	}
	// One key per purchase, reused only to retry this one, so a retry can never
	// pay twice.
	idempotencyKey := uuid.NewString()

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, a.api+"/v1/payments/authorize", bytes.NewReader(body))
		if err != nil {
			return decision{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+a.key)
		req.Header.Set("Idempotency-Key", idempotencyKey)

		started := time.Now()
		res, err := a.http.Do(req)
		if err != nil {
			// Fail closed: no answer means no payment.
			return decision{}, fmt.Errorf("cleargate unreachable, not paying: %w", err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()

		if res.StatusCode == http.StatusOK {
			var d decision
			if err := json.Unmarshal(raw, &d); err != nil {
				return decision{}, err
			}
			fmt.Printf("  → Cleargate decided in %d ms\n", time.Since(started).Milliseconds())
			return d, nil
		}
		// The one 409 to retry with the same key: an identical transaction is
		// already out for another payment.
		if res.StatusCode == http.StatusConflict && strings.Contains(string(raw), "transaction_in_flight") && attempt < 3 {
			wait, _ := strconv.Atoi(res.Header.Get("Retry-After"))
			time.Sleep(time.Duration(max(wait, 1)) * time.Second)
			continue
		}
		return decision{}, fmt.Errorf("cleargate answered %d, not paying: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
}

// reply is a merchant's answer, read in full.
type reply struct {
	status int
	header http.Header
	body   string
}

func (a *agent) get(url, paymentSignature string) (reply, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return reply{}, err
	}
	if paymentSignature != "" {
		req.Header.Set("PAYMENT-SIGNATURE", paymentSignature)
	}
	res, err := a.http.Do(req)
	if err != nil {
		return reply{}, fmt.Errorf("merchant unreachable: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	return reply{status: res.StatusCode, header: res.Header, body: strings.TrimSpace(string(raw))}, err
}

// decodeHeader reads an x402 header: base64 of JSON.
func decodeHeader(v string) (json.RawMessage, error) {
	if v == "" {
		return nil, errors.New("header missing")
	}
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, errors.New("not JSON")
	}
	return raw, nil
}

// describe is the first option's price and payee, for the narration only.
// Cleargate reads the requirement itself and never trusts the agent's reading.
func describe(requirement json.RawMessage) (price, payTo string) {
	var r struct {
		Accepts []struct {
			Amount string `json:"amount"`
			PayTo  string `json:"payTo"`
		} `json:"accepts"`
	}
	if json.Unmarshal(requirement, &r) != nil || len(r.Accepts) == 0 {
		return "?", "?"
	}
	atomic, err := strconv.ParseInt(r.Accepts[0].Amount, 10, 64)
	if err != nil {
		return r.Accepts[0].Amount + " atomic", r.Accepts[0].PayTo
	}
	price = strings.TrimRight(fmt.Sprintf("%d.%06d", atomic/1_000_000, atomic%1_000_000), "0")
	return strings.TrimSuffix(price, "."), r.Accepts[0].PayTo
}

func short(address string) string {
	if len(address) <= 12 {
		return address
	}
	return address[:4] + "…" + address[len(address)-4:]
}

// loadEnv reads the KEY=VALUE file console setup writes. A variable already in
// the environment wins over the file.
func loadEnv(path string) func(name, fallback string) string {
	values := map[string]string{}
	if f, err := os.Open(path); err == nil { // #nosec G304 -- the demo's own state file
		defer func() { _ = f.Close() }()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			if k, v, ok := strings.Cut(scanner.Text(), "="); ok {
				values[k] = v
			}
		}
	}
	return func(name, fallback string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		if v := values[name]; v != "" {
			return v
		}
		return fallback
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
