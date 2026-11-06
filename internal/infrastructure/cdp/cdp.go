// Package cdp talks to Coinbase Developer Platform, which holds account
// wallets' keys as Server Wallets (whitepaper §8.8; the owner's decision of 5
// November 2026). No private key ever reaches this process: CDP keeps it in
// its enclave and signs on request.
//
// It knows nothing about accounts or settlement. internal/app adapts it to
// settlement.WalletProvider, so CDP's SDK stays inside this package and
// replacing the custodian touches nothing that authorizes payments.
package cdp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/coinbase/cdp-sdk/go"
	"github.com/coinbase/cdp-sdk/go/openapi"
)

// Options configures a Client.
type Options struct {
	// APIKeyID and APIKeySecret authenticate every request. WalletSecret
	// authorizes the ones that create accounts or sign, which is why it is a
	// separate secret: a leaked API key alone can read but not move money.
	APIKeyID     string
	APIKeySecret string
	WalletSecret string

	// BaseURL is CDP's API root. Empty selects CDP's production API, which
	// serves devnet and mainnet alike; tests point it at a fake.
	BaseURL string

	// Timeout bounds each call. Zero selects ten seconds. The SDK's HTTP
	// client has none of its own, so without this a hung connection would
	// hold a payment's signing step open indefinitely.
	Timeout time.Duration
}

// Client calls CDP's Solana Server Wallet API.
type Client struct {
	api     *openapi.ClientWithResponses
	timeout time.Duration
}

// ErrMissingCredentials reports a Client built without all three secrets.
var ErrMissingCredentials = errors.New("cdp: CDP_API_KEY_ID, CDP_API_KEY_SECRET and CDP_WALLET_SECRET are all required")

// New builds a Client. It refuses missing credentials rather than failing at
// the first payment, when the account is already waiting on a signature.
func New(o Options) (*Client, error) {
	if o.APIKeyID == "" || o.APIKeySecret == "" || o.WalletSecret == "" {
		return nil, ErrMissingCredentials
	}
	api, err := sdk.NewClient(sdk.ClientOptions{
		APIKeyID: o.APIKeyID, APIKeySecret: o.APIKeySecret, WalletSecret: o.WalletSecret,
		BasePath: o.BaseURL,
	})
	if err != nil {
		return nil, fmt.Errorf("cdp: %w", err)
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &Client{api: api, timeout: timeout}, nil
}

// SolanaAccount returns the address of the project's Solana account called
// name, creating it if there is none.
//
// The name is what makes this idempotent. CDP keeps account names unique in a
// project, so a second create, a concurrent one or a retry after a lost
// response gets 409 and reads the existing account back instead of making
// another. Names are 2 to 36 letters, digits and hyphens.
func (c *Client) SolanaAccount(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	created, err := c.api.CreateSolanaAccountWithResponse(ctx,
		&openapi.CreateSolanaAccountParams{}, openapi.CreateSolanaAccountJSONRequestBody{Name: &name})
	if err != nil {
		return "", fmt.Errorf("cdp: create solana account: %w", err)
	}
	switch {
	case created.JSON201 != nil:
		return created.JSON201.Address, nil
	case created.StatusCode() != http.StatusConflict:
		return "", apiError("create solana account", created.StatusCode(), created.Body)
	}

	existing, err := c.api.GetSolanaAccountByNameWithResponse(ctx, name)
	if err != nil {
		return "", fmt.Errorf("cdp: get solana account: %w", err)
	}
	if existing.JSON200 == nil {
		return "", apiError("get solana account", existing.StatusCode(), existing.Body)
	}
	return existing.JSON200.Address, nil
}

// SignSolanaTransaction signs a serialized transaction with the key behind
// address and returns the transaction with that signature added. It does not
// send it: in an x402 `exact` payment the merchant's facilitator pays the fee
// and submits (Task 7.6).
func (c *Client) SignSolanaTransaction(ctx context.Context, address string, tx []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.api.SignSolanaTransactionWithResponse(ctx, address,
		&openapi.SignSolanaTransactionParams{},
		openapi.SignSolanaTransactionJSONRequestBody{Transaction: base64.StdEncoding.EncodeToString(tx)})
	if err != nil {
		return nil, fmt.Errorf("cdp: sign solana transaction: %w", err)
	}
	if resp.JSON200 == nil {
		return nil, apiError("sign solana transaction", resp.StatusCode(), resp.Body)
	}
	signed, err := base64.StdEncoding.DecodeString(resp.JSON200.SignedTransaction)
	if err != nil {
		return nil, fmt.Errorf("cdp: sign solana transaction: response is not base64: %w", err)
	}
	return signed, nil
}

// apiError describes a refused call by CDP's own error type and correlation
// id, which is what CDP support asks for. Nothing from the request is echoed.
func apiError(op string, status int, body []byte) error {
	var e openapi.Error
	if json.Unmarshal(body, &e) != nil || e.ErrorType == "" {
		return fmt.Errorf("cdp: %s: status %d", op, status)
	}
	correlation := ""
	if e.CorrelationId != nil {
		correlation = ", correlation id " + *e.CorrelationId
	}
	return fmt.Errorf("cdp: %s: status %d, %s: %s%s", op, status, e.ErrorType, e.ErrorMessage, correlation)
}
