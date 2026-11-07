// Package solana reads the Solana chain over JSON-RPC: what landed, what a
// transaction did to a token balance, and how far the chain has got.
//
// Every read is at `finalized`. A deposit credited or a payment settled on
// anything less could be rolled back by the chain after the ledger has already
// moved, and the ledger cannot follow it there.
//
// It knows nothing about accounts or payments. internal/app adapts it to the
// ports the modules declare, so solana-go stays inside this package and no
// module that authorizes payments ever learns what a chain is.
package solana

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// Options configures a Client.
type Options struct {
	// RPCURL is the JSON-RPC endpoint. It decides which network this client
	// reads, so it must agree with SOLANA_NETWORK.
	RPCURL string

	// Timeout bounds each call. Zero selects ten seconds. solana-go's HTTP
	// client has none of its own, so a hung connection would otherwise stall
	// a worker job for good.
	Timeout time.Duration
}

// Client reads the chain.
type Client struct {
	rpc     *rpc.Client
	url     string
	timeout time.Duration
}

// ErrMissingRPCURL reports a Client built with no endpoint.
var ErrMissingRPCURL = errors.New("solana: SOLANA_RPC_URL is required")

// ErrTransactionNotFound reports a signature the node has no finalized
// transaction for.
var ErrTransactionNotFound = errors.New("solana: transaction not found")

// New builds a Client.
func New(o Options) (*Client, error) {
	if o.RPCURL == "" {
		return nil, ErrMissingRPCURL
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	return &Client{rpc: rpc.New(o.RPCURL), url: o.RPCURL, timeout: timeout}, nil
}

// FinalizedHeight is the block height the chain has finalized up to. A
// blockhash stops being usable once this passes its last valid height.
func (c *Client) FinalizedHeight(ctx context.Context) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	height, err := c.rpc.GetBlockHeight(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return 0, fmt.Errorf("solana: block height: %w", err)
	}
	return height, nil
}

// LastValidBlockHeight is the last height at which the newest finalized
// blockhash can land.
func (c *Client) LastValidBlockHeight(ctx context.Context) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	latest, err := c.rpc.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return 0, fmt.Errorf("solana: latest blockhash: %w", err)
	}
	return latest.Value.LastValidBlockHeight, nil
}

// Page selects a range of an address's history, newest first.
type Page struct {
	// Before starts the page below this signature; empty starts at the newest.
	Before string
	// Until stops the page at this signature, excluding it.
	Until string
	// Limit is the page size; zero selects the node's maximum of 1,000.
	Limit int
}

// Signature is one finalized transaction that touched an address.
type Signature struct {
	Signature string
	// Failed is a transaction that landed and did nothing but pay its fee.
	Failed bool
	// Memo is the node's rendering of the transaction's memos, empty if none.
	// Nodes prefix each with its length, so match it with strings.Contains.
	Memo string
	// BlockTime is zero when the node does not know it.
	BlockTime time.Time
}

// Signatures returns one page of the finalized transactions touching address,
// newest first.
func (c *Client) Signatures(ctx context.Context, address string, p Page) ([]Signature, error) {
	key, err := sol.PublicKeyFromBase58(address)
	if err != nil {
		return nil, fmt.Errorf("solana: address: %w", err)
	}
	opts := &rpc.GetSignaturesForAddressOpts{Commitment: rpc.CommitmentFinalized}
	if p.Limit > 0 {
		limit := p.Limit
		opts.Limit = &limit
	}
	if p.Before != "" {
		if opts.Before, err = sol.SignatureFromBase58(p.Before); err != nil {
			return nil, fmt.Errorf("solana: before: %w", err)
		}
	}
	if p.Until != "" {
		if opts.Until, err = sol.SignatureFromBase58(p.Until); err != nil {
			return nil, fmt.Errorf("solana: until: %w", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	found, err := c.rpc.GetSignaturesForAddressWithOpts(ctx, key, opts)
	if err != nil {
		return nil, fmt.Errorf("solana: signatures for address: %w", err)
	}

	out := make([]Signature, len(found))
	for i, s := range found {
		out[i] = Signature{Signature: s.Signature.String(), Failed: s.Err != nil}
		if s.Memo != nil {
			out[i].Memo = *s.Memo
		}
		if s.BlockTime != nil {
			out[i].BlockTime = s.BlockTime.Time()
		}
	}
	return out, nil
}

// Transaction is what one finalized transaction did.
type Transaction struct {
	// Message is the serialized message, the bytes every signer signed. It is
	// the same whoever submitted the transaction and however many signatures
	// it collected, so it identifies a transaction whose id we never saw.
	Message []byte

	// Failed is a transaction that landed and did nothing but pay its fee.
	Failed bool

	// changes is the net token movement per owner and mint.
	changes map[tokenHolding]*big.Int
}

type tokenHolding struct{ owner, mint string }

// TokenChange is how much of mint the owner's token accounts gained in this
// transaction, in atomic units, negative for a loss.
//
// Reading the balance change rather than the transfer instructions counts a
// transfer made through another program the same as a direct one, and needs
// no account table to resolve an address-lookup transaction.
func (t Transaction) TokenChange(owner, mint string) *big.Int {
	if change, ok := t.changes[tokenHolding{owner, mint}]; ok {
		return new(big.Int).Set(change)
	}
	return new(big.Int)
}

// Transaction reads a finalized transaction, or reports ErrTransactionNotFound.
func (c *Client) Transaction(ctx context.Context, signature string) (Transaction, error) {
	sig, err := sol.SignatureFromBase58(signature)
	if err != nil {
		return Transaction{}, fmt.Errorf("solana: signature: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// Without this version the node refuses to return a v0 transaction, which
	// is what x402 `exact` payments are.
	version := rpc.MaxSupportedTransactionVersion0
	res, err := c.rpc.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{
		Encoding:                       sol.EncodingBase64,
		Commitment:                     rpc.CommitmentFinalized,
		MaxSupportedTransactionVersion: &version,
	})
	if errors.Is(err, rpc.ErrNotFound) {
		return Transaction{}, ErrTransactionNotFound
	}
	if err != nil {
		return Transaction{}, fmt.Errorf("solana: transaction: %w", err)
	}
	if res == nil || res.Transaction == nil || res.Meta == nil {
		return Transaction{}, ErrTransactionNotFound
	}

	parsed, err := res.Transaction.GetTransaction()
	if err != nil {
		return Transaction{}, fmt.Errorf("solana: decode transaction: %w", err)
	}
	message, err := parsed.Message.MarshalBinary()
	if err != nil {
		return Transaction{}, fmt.Errorf("solana: encode message: %w", err)
	}

	out := Transaction{Message: message, Failed: res.Meta.Err != nil, changes: map[tokenHolding]*big.Int{}}
	if err := out.apply(res.Meta.PreTokenBalances, -1); err != nil {
		return Transaction{}, err
	}
	if err := out.apply(res.Meta.PostTokenBalances, 1); err != nil {
		return Transaction{}, err
	}
	return out, nil
}

// apply adds balances to the net change, negated for the balances before.
func (t Transaction) apply(balances []rpc.TokenBalance, sign int64) error {
	for _, b := range balances {
		if b.Owner == nil || b.UiTokenAmount == nil {
			continue
		}
		amount, ok := new(big.Int).SetString(b.UiTokenAmount.Amount, 10)
		if !ok {
			return fmt.Errorf("solana: token balance %q is not an integer", b.UiTokenAmount.Amount)
		}
		key := tokenHolding{b.Owner.String(), b.Mint.String()}
		if t.changes[key] == nil {
			t.changes[key] = new(big.Int)
		}
		t.changes[key].Add(t.changes[key], amount.Mul(amount, big.NewInt(sign)))
	}
	return nil
}

// TokenAccount is the associated token account that holds mint for owner: the
// address a USDC transfer to owner actually lands in.
func TokenAccount(owner, mint string) (string, error) {
	ownerKey, err := sol.PublicKeyFromBase58(owner)
	if err != nil {
		return "", fmt.Errorf("solana: owner: %w", err)
	}
	mintKey, err := sol.PublicKeyFromBase58(mint)
	if err != nil {
		return "", fmt.Errorf("solana: mint: %w", err)
	}
	account, _, err := sol.FindAssociatedTokenAddress(ownerKey, mintKey)
	if err != nil {
		return "", fmt.Errorf("solana: token account: %w", err)
	}
	return account.String(), nil
}

// MessageOf is the message of a base64-encoded signed transaction, in the form
// Transaction.Message reports, so the two can be compared.
func MessageOf(signedTx string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(signedTx)
	if err != nil {
		return nil, fmt.Errorf("solana: transaction is not base64: %w", err)
	}
	tx, err := sol.TransactionFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("solana: decode transaction: %w", err)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("solana: encode message: %w", err)
	}
	return message, nil
}
