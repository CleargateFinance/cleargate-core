// Package devnet is the demo's help with Solana devnet: it asks CDP's faucet
// for USDC, checks whether a wallet can receive USDC yet, and reads the CDP key
// from the repository's .env.
//
// It is the demo's convenience, not Cleargate's. Cleargate itself never asks a
// faucet for anything; it only credits what arrives in an account's wallet.
package devnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/coinbase/cdp-sdk/go"
	"github.com/coinbase/cdp-sdk/go/openapi"
	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

const (
	// Network is Solana devnet, the network Cleargate's devnet configuration
	// pays on, as a CAIP-2 id.
	Network = "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"

	// USDCMint is Circle's devnet USDC, the only token Cleargate pays in there.
	USDCMint = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"

	// FaucetDailyLimit is how many USDC CDP's faucet sends in a rolling 24
	// hours, 1 a request, counted per CDP project and per address.
	FaucetDailyLimit = 10
)

var (
	// ErrNoCDPKey is a faucet asked for without a CDP API key.
	ErrNoCDPKey = errors.New("no CDP_API_KEY_ID and CDP_API_KEY_SECRET in the environment or .env")

	// ErrFaucetLimit is CDP's faucet refusing more USDC for now.
	ErrFaucetLimit = errors.New("CDP's devnet faucet has reached its daily limit")
)

// Faucet asks CDP's devnet faucet for USDC.
type Faucet struct {
	api *openapi.ClientWithResponses
}

// NewFaucet builds a faucet client with a CDP project's API key. The faucet
// moves CDP's funds, not a wallet's, so it needs no wallet secret. An empty
// baseURL selects CDP's production API, which serves devnet.
func NewFaucet(apiKeyID, apiKeySecret, baseURL string) (*Faucet, error) {
	if apiKeyID == "" || apiKeySecret == "" {
		return nil, ErrNoCDPKey
	}
	api, err := sdk.NewClient(sdk.ClientOptions{APIKeyID: apiKeyID, APIKeySecret: apiKeySecret, BasePath: baseURL})
	if err != nil {
		return nil, fmt.Errorf("cdp: %w", err)
	}
	return &Faucet{api: api}, nil
}

// SendUSDC asks for 1 devnet USDC to address and returns the transaction's
// signature. Sending to a wallet with no USDC token account creates it.
func (f *Faucet) SendUSDC(ctx context.Context, address string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := f.api.RequestSolanaFaucetWithResponse(ctx,
		openapi.RequestSolanaFaucetJSONRequestBody{Address: address, Token: openapi.Usdc})
	if err != nil {
		return "", fmt.Errorf("CDP faucet: %w", err)
	}
	switch {
	case resp.JSON200 != nil:
		return resp.JSON200.TransactionSignature, nil
	case resp.StatusCode() == http.StatusTooManyRequests:
		return "", ErrFaucetLimit
	default:
		return "", faucetError(resp.StatusCode(), resp.Body)
	}
}

// faucetError names CDP's error type and correlation id, which is what CDP's
// support asks for.
func faucetError(status int, body []byte) error {
	var e openapi.Error
	if json.Unmarshal(body, &e) != nil || e.ErrorType == "" {
		return fmt.Errorf("CDP faucet: status %d", status)
	}
	correlation := ""
	if e.CorrelationId != nil {
		correlation = ", correlation id " + *e.CorrelationId
	}
	return fmt.Errorf("CDP faucet: status %d, %s: %s%s", status, e.ErrorType, e.ErrorMessage, correlation)
}

// HasUSDCAccount reports whether owner's associated USDC token account exists.
// An x402 `exact` payment carries only the transfer, never an instruction to
// create the receiving account, so every payment to a wallet without one fails.
func HasUSDCAccount(ctx context.Context, rpcURL, owner string) (bool, error) {
	ownerKey, err := sol.PublicKeyFromBase58(owner)
	if err != nil {
		return false, fmt.Errorf("%q is not a Solana address: %w", owner, err)
	}
	ata, _, err := sol.FindAssociatedTokenAddress(ownerKey, sol.MustPublicKeyFromBase58(USDCMint))
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err = rpc.New(rpcURL).GetAccountInfo(ctx, ata)
	switch {
	case errors.Is(err, rpc.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read %s's USDC account: %w", owner, err)
	default:
		return true, nil
	}
}

// Explorer links a devnet address or transaction in Solana Explorer.
func Explorer(kind, id string) string {
	return "https://explorer.solana.com/" + kind + "/" + id + "?cluster=devnet"
}
