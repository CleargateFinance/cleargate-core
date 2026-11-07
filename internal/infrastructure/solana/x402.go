package solana

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	sol "github.com/gagliardetto/solana-go"
	"github.com/x402-foundation/x402/go/mechanisms/svm"
	exact "github.com/x402-foundation/x402/go/mechanisms/svm/exact/client"
	"github.com/x402-foundation/x402/go/types"
)

// signingBudget bounds building and signing one transfer: two reads of the
// chain and a call to the custodian. It sits inside the API's write timeout, so
// a slow node fails the request cleanly rather than losing the response after
// the signature exists.
const signingBudget = 8 * time.Second

// ExactTransfer is one x402 `exact` payment to build and sign.
type ExactTransfer struct {
	// Owner is the wallet paying. It signs, and holds no SOL: FeePayer does.
	Owner string
	// FeePayer is the merchant's facilitator, who pays the fee and submits.
	FeePayer string

	Network string
	Mint    string
	PayTo   string
	// Amount is in the mint's atomic units.
	Amount string
	// Memo goes on-chain, and is how the transaction is found again.
	Memo string

	// Sign adds Owner's signature to a serialized transaction and returns it.
	Sign func(ctx context.Context, tx []byte) ([]byte, error)
}

// SignedExact is a transaction ready to hand to a merchant.
type SignedExact struct {
	// Transaction is base64, partially signed: the fee payer's slot is empty.
	Transaction string

	// LastValidBlockHeight is the height after which it can never land. The
	// SDK does not report its blockhash's own figure, so this comes from a read
	// after the SDK's: a later blockhash lives at least as long, so the figure
	// can only err late, and releasing funds late is the safe side.
	LastValidBlockHeight uint64
}

// SignExact builds the transfer with the x402 SDK, which is what guarantees the
// instruction layout a facilitator accepts, and signs it through t.Sign. The SDK
// signer helpers take a private key; the custodian's is never in this process.
func (c *Client) SignExact(ctx context.Context, t ExactTransfer) (SignedExact, error) {
	owner, err := sol.PublicKeyFromBase58(t.Owner)
	if err != nil {
		return SignedExact{}, fmt.Errorf("solana: owner: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, signingBudget)
	defer cancel()

	scheme := exact.NewExactSvmScheme(custodySigner{owner: owner, sign: t.Sign}, &svm.ClientConfig{RPCURL: c.url})
	payload, err := scheme.CreatePaymentPayload(ctx, types.PaymentRequirements{
		Scheme:  svm.SchemeExact,
		Network: t.Network,
		Asset:   t.Mint,
		Amount:  t.Amount,
		PayTo:   t.PayTo,
		Extra:   map[string]any{"feePayer": t.FeePayer, "memo": t.Memo},
	})
	if err != nil {
		return SignedExact{}, fmt.Errorf("solana: build exact transfer: %w", err)
	}
	transaction, ok := payload.Payload["transaction"].(string)
	if !ok || transaction == "" {
		return SignedExact{}, errors.New("solana: the x402 SDK returned no transaction")
	}

	lastValid, err := c.LastValidBlockHeight(ctx)
	if err != nil {
		return SignedExact{}, err
	}
	return SignedExact{Transaction: transaction, LastValidBlockHeight: lastValid}, nil
}

// custodySigner signs through the custodian rather than with a key in memory.
type custodySigner struct {
	owner sol.PublicKey
	sign  func(ctx context.Context, tx []byte) ([]byte, error)
}

// Address implements svm.ClientSvmSigner.
func (s custodySigner) Address() sol.PublicKey { return s.owner }

// SignTransaction implements svm.ClientSvmSigner.
//
// What comes back is checked rather than trusted: the message must be the one
// sent, and the signature must verify against the owner. This is the last point
// before a transaction leaves for a merchant, and a custodian that signed
// something else must fail here, not on-chain.
func (s custodySigner) SignTransaction(ctx context.Context, tx *sol.Transaction) error {
	required := int(tx.Message.Header.NumRequiredSignatures)
	slot := -1
	for i := range required {
		if tx.Message.AccountKeys[i].Equals(s.owner) {
			slot = i
		}
	}
	if slot < 0 {
		return errors.New("solana: the wallet is not a signer of this transfer")
	}

	// One empty slot per signer is the wire format of a partly signed
	// transaction, and it keeps the fee payer's slot open.
	tx.Signatures = make([]sol.Signature, required)
	unsigned, err := tx.MarshalBinary()
	if err != nil {
		return fmt.Errorf("solana: encode transaction: %w", err)
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return fmt.Errorf("solana: encode message: %w", err)
	}

	raw, err := s.sign(ctx, unsigned)
	if err != nil {
		return err
	}
	signed, err := sol.TransactionFromBytes(raw)
	if err != nil {
		return fmt.Errorf("solana: decode signed transaction: %w", err)
	}
	got, err := signed.Message.MarshalBinary()
	if err != nil {
		return fmt.Errorf("solana: encode signed message: %w", err)
	}
	if !bytes.Equal(got, message) {
		return errors.New("solana: the custodian changed the transaction it was asked to sign")
	}
	if len(signed.Signatures) != required || !signed.Signatures[slot].Verify(s.owner, message) {
		return errors.New("solana: the custodian's signature does not verify")
	}

	tx.Signatures[slot] = signed.Signatures[slot]
	return nil
}
