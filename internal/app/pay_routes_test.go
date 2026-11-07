//go:build integration

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sol "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana/solanatest"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// These tests pay through the real API: the agent's key, a fake custodian that
// signs with real keys, and a fake node. Nothing between them is stubbed.

const payMerchant = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"

type authorizeJSON struct {
	PaymentID        string  `json:"payment_id"`
	Status           string  `json:"status"`
	Outcome          string  `json:"outcome"`
	ReasonCode       string  `json:"reason_code"`
	Replayed         bool    `json:"replayed"`
	PaymentSignature *string `json:"payment_signature"`
}

// payFixture is an account with a wallet, a balance and an agent mandated to
// pay 20 at a time, 100 a day, in data_api.
type payFixture struct {
	*apiFixture
	wallet   string
	feePayer string
}

func newPayFixture(t *testing.T, balance string) *payFixture {
	t.Helper()
	f := newAPIFixture(t)
	owner := f.session("owner@example.com", "owner", true)

	rec := f.do(http.MethodPut, "/v1/agents/"+string(f.agentID)+"/mandate", rulesJSON,
		credentials{sessionToken: owner, consoleHeader: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = f.do(http.MethodGet, "/v1/accounts/"+string(f.accountID)+"/deposit-address", "", credentials{sessionToken: owner})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	wallet := decode[depositAddressJSON](t, rec.Body.Bytes()).Address

	ledgerSvc := ledger.New(f.db.DB, nil).Service()
	funds, err := ledgerSvc.EnsureAccount(context.Background(), &f.accountID, ledger.TypeCustomerFunds, currency.USD)
	require.NoError(t, err)
	float, err := ledgerSvc.EnsureAccount(context.Background(), nil, ledger.TypeOnchainFloat, currency.USD)
	require.NoError(t, err)
	_, err = ledgerSvc.Post(context.Background(), ledger.Entry{
		Kind: ledger.KindFunding, Asset: currency.USD, Lines: ledger.Transfer(float, funds, usd(t, balance)),
	})
	require.NoError(t, err)

	return &payFixture{apiFixture: f, wallet: wallet, feePayer: solanatest.RandomAddress(t)}
}

// body is a request paying atomic units of USDC, in the given category.
func (f *payFixture) body(atomic, category string) string {
	return fmt.Sprintf(`{"category":%q,"purpose":"price lookup","url_source":"task","requirement":{
		"x402Version":2,"resource":{"url":"https://api.example.com/v1/quote"},
		"accepts":[{"scheme":"exact","network":%q,"asset":%q,"amount":%q,"payTo":%q,"maxTimeoutSeconds":60,
		            "extra":{"feePayer":%q}}]}}`,
		category, devnetRail.Network, solanatest.DevnetUSDC, atomic, payMerchant, f.feePayer)
}

func (f *payFixture) authorize(key, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/payments/authorize", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.agentKey)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	f.api.Engine.ServeHTTP(rec, req)
	f.conform(req, rec)
	return rec
}

func (f *payFixture) pays(key, body string) authorizeJSON {
	f.t.Helper()
	rec := f.authorize(key, body)
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	return decode[authorizeJSON](f.t, rec.Body.Bytes())
}

func (f *payFixture) balance() string {
	f.t.Helper()
	b, err := ledger.New(f.db.DB, nil).Service().AccountBalance(context.Background(), f.accountID, currency.USD)
	require.NoError(f.t, err)
	return b.String()
}

func (f *payFixture) rows(query string) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(context.Background(), query).Scan(&n))
	return n
}

// transaction opens a PAYMENT-SIGNATURE down to the transaction inside it.
func (f *payFixture) transaction(signature string) (*sol.Transaction, map[string]json.RawMessage) {
	f.t.Helper()
	raw, err := base64.StdEncoding.DecodeString(signature)
	require.NoError(f.t, err)
	var payload map[string]json.RawMessage
	require.NoError(f.t, json.Unmarshal(raw, &payload))
	var inner struct {
		Transaction string `json:"transaction"`
	}
	require.NoError(f.t, json.Unmarshal(payload["payload"], &inner))
	tx, err := sol.TransactionFromBase64(inner.Transaction)
	require.NoError(f.t, err)
	return tx, payload
}

// TestPay_AnApprovalDebitsAndSigns is the path the whole product exists for.
func TestPay_AnApprovalDebitsAndSigns(t *testing.T) {
	f := newPayFixture(t, "50")

	got := f.pays("key-1", f.body("400000", "data_api")) // $0.40

	assert.Equal(t, "authorized", got.Status)
	assert.Equal(t, "approved", got.Outcome)
	assert.Equal(t, "ok", got.ReasonCode)
	assert.False(t, got.Replayed)
	require.NotNil(t, got.PaymentSignature)
	assert.Equal(t, "49.6", f.balance())

	tx, payload := f.transaction(*got.PaymentSignature)
	message, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	wallet := sol.MustPublicKeyFromBase58(f.wallet)
	require.Len(t, tx.Signatures, 2)
	assert.True(t, tx.Signatures[0].IsZero(), "the facilitator signs as fee payer")
	assert.True(t, tx.Signatures[1].Verify(wallet, message), "the account's wallet signed")

	var memo string
	for _, ix := range tx.Message.Instructions {
		program, err := tx.ResolveProgramIDIndex(ix.ProgramIDIndex)
		require.NoError(t, err)
		if program.Equals(sol.MemoProgramID) {
			memo = string(ix.Data)
		}
	}
	assert.Equal(t, got.PaymentID, memo, "the memo is what Task 7.7 finds it by")
	assert.Contains(t, string(payload["accepted"]), payMerchant)
	assert.Contains(t, string(payload["resource"]), "api.example.com")

	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM payment_transfer`))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM decision WHERE outcome = 'approved'`))
	var counterparty, resource string
	require.NoError(t, f.db.QueryRow(context.Background(),
		`SELECT counterparty_id, resource_url FROM payment`).Scan(&counterparty, &resource))
	assert.NotEmpty(t, counterparty, "the payee is resolved from the requirement's resource")
	assert.Equal(t, "https://api.example.com/v1/quote", resource)
}

// TestPay_ADeclineSignsNothing: $85 against a $20 cap, from the definition of
// done. No money moves and nothing reaches the custodian.
func TestPay_ADeclineSignsNothing(t *testing.T) {
	f := newPayFixture(t, "100")

	got := f.pays("key-1", f.body("85000000", "data_api"))

	assert.Equal(t, "declined", got.Status)
	assert.Equal(t, "over_per_tx_cap", got.ReasonCode)
	assert.Nil(t, got.PaymentSignature)
	assert.Equal(t, "100", f.balance())
	assert.Zero(t, f.cdp.Signs())
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM decision WHERE reason_code = 'over_per_tx_cap'`))
	assert.Zero(t, f.rows(`SELECT count(*) FROM payment_transfer`))
}

func TestPay_ACategoryOutsideTheMandateIsDeclined(t *testing.T) {
	f := newPayFixture(t, "100")

	got := f.pays("key-1", f.body("1000000", "gambling"))

	assert.Equal(t, "category_not_permitted", got.ReasonCode)
	assert.Nil(t, got.PaymentSignature)
	assert.Zero(t, f.cdp.Signs())
}

// TestPay_ARetryReturnsTheSameTransaction: a timed-out request retried with its
// key pays once, and hands out the one transaction that exists. A second
// signature would be a second transaction that could also land.
func TestPay_ARetryReturnsTheSameTransaction(t *testing.T) {
	f := newPayFixture(t, "50")

	first := f.pays("key-1", f.body("1000000", "data_api"))
	retry := f.pays("key-1", f.body("1000000", "data_api"))

	assert.True(t, retry.Replayed)
	assert.Equal(t, first.PaymentID, retry.PaymentID)
	require.NotNil(t, retry.PaymentSignature)
	assert.Equal(t, *first.PaymentSignature, *retry.PaymentSignature)
	assert.Equal(t, 1, f.cdp.Signs())
	assert.Equal(t, "49", f.balance())
}

func TestPay_AKeyReusedForAnotherPaymentConflicts(t *testing.T) {
	f := newPayFixture(t, "50")
	f.pays("key-1", f.body("1000000", "data_api"))

	rec := f.authorize("key-1", f.body("2000000", "data_api"))

	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Equal(t, "49", f.balance())
}

// TestPay_ConcurrentRetriesAgreeOnOneTransaction: every copy of a request gets
// the same transaction, whichever signing won, and the account pays once.
func TestPay_ConcurrentRetriesAgreeOnOneTransaction(t *testing.T) {
	f := newPayFixture(t, "50")

	const n = 8
	results := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/payments/authorize", strings.NewReader(f.body("1000000", "data_api")))
			req.Header.Set("Authorization", "Bearer "+f.agentKey)
			req.Header.Set("Idempotency-Key", "key-1")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			f.api.Engine.ServeHTTP(rec, req)
			results[i] = rec
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, rec := range results {
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := decode[authorizeJSON](t, rec.Body.Bytes())
		require.NotNil(t, got.PaymentSignature)
		seen[*got.PaymentSignature] = true
	}
	assert.Len(t, seen, 1)
	assert.Equal(t, "49", f.balance())
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM payment`))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM payment_transfer`))
}

// TestPay_ASigningFailureCanBeRetried: the payment is approved and debited, the
// custodian is down, and the agent is told it did not happen. Nobody holds a
// transaction, so the same key signing again is safe, and it pays once.
func TestPay_ASigningFailureCanBeRetried(t *testing.T) {
	f := newPayFixture(t, "50")
	f.cdp.FailSigning(true)

	rec := f.authorize("key-1", f.body("1000000", "data_api"))
	assert.GreaterOrEqual(t, rec.Code, 500, rec.Body.String())
	assert.Equal(t, "49", f.balance(), "debited, and held until a retry or the sweep")
	assert.Zero(t, f.rows(`SELECT count(*) FROM payment_transfer`))

	f.cdp.FailSigning(false)
	got := f.pays("key-1", f.body("1000000", "data_api"))

	assert.True(t, got.Replayed)
	require.NotNil(t, got.PaymentSignature)
	assert.Equal(t, "49", f.balance(), "charged once")
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM payment`))
}

// TestPay_ARequirementItCannotPayIsARefusal: nothing is decided, recorded or
// debited, and no key is remembered, so the agent can try another merchant.
func TestPay_ARequirementItCannotPayIsARefusal(t *testing.T) {
	f := newPayFixture(t, "50")

	for name, body := range map[string]string{
		"not json":         `nope`,
		"no requirement":   `{"category":"data_api"}`,
		"another chain":    strings.ReplaceAll(f.body("1000000", "data_api"), devnetRail.Network, "eip155:8453"),
		"another token":    strings.ReplaceAll(f.body("1000000", "data_api"), solanatest.DevnetUSDC, "So11111111111111111111111111111111111111112"),
		"malformed amount": f.body("1.5", "data_api"),
		"zero":             f.body("0", "data_api"),
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.authorize("key-1", body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
	assert.Zero(t, f.rows(`SELECT count(*) FROM payment`))
	assert.Zero(t, f.cdp.Signs())
	assert.Equal(t, "50", f.balance())
}

func TestPay_NeedsAnIdempotencyKey(t *testing.T) {
	f := newPayFixture(t, "50")

	rec := f.authorize("", f.body("1000000", "data_api"))

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Zero(t, f.rows(`SELECT count(*) FROM payment`))
}

// TestPay_IsOnlyOnTheAgentSurface: a person's session cannot reach the route.
func TestPay_IsOnlyOnTheAgentSurface(t *testing.T) {
	f := newPayFixture(t, "50")
	session := f.session("admin@example.com", "admin", true)

	rec := f.do(http.MethodPost, "/v1/payments/authorize", f.body("1000000", "data_api"),
		credentials{sessionToken: session, consoleHeader: true})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Zero(t, f.rows(`SELECT count(*) FROM payment`))
}

// TestPayment_TransferSchemaRefuses shows what the database refuses on its own.
func TestPayment_TransferSchemaRefuses(t *testing.T) {
	f := newPayFixture(t, "100")
	ctx := context.Background()
	approved := f.pays("key-1", f.body("1000000", "data_api"))
	declined := f.pays("key-2", f.body("85000000", "data_api"))
	unsigned := f.rows(`SELECT count(*) FROM payment_transfer`)
	require.Equal(t, 1, unsigned)

	insert := func(paymentID string, height int) error {
		_, err := f.db.Exec(ctx, `
			INSERT INTO payment_transfer (payment_id, memo, wallet_address, signed_transaction, payment_signature, last_valid_block_height)
			VALUES ($1, 'memo', $2, 'tx', 'sig', $3)`, paymentID, f.wallet, height)
		return err
	}

	assert.ErrorContains(t, insert(declined.PaymentID, 1), "cannot gain a transaction", "a declined payment")
	assert.ErrorContains(t, insert(approved.PaymentID, 1), "payment_transfer_pkey", "a second transaction")
	_, err := f.db.Exec(ctx, `UPDATE payment_transfer SET memo = 'other'`)
	assert.ErrorContains(t, err, "append-only")
	_, err = f.db.Exec(ctx, `DELETE FROM payment_transfer`)
	assert.ErrorContains(t, err, "append-only")
}
