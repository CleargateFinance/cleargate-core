package settlement

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// TransferRequest is an approved payment to realize.
type TransferRequest struct {
	AccountID id.AccountID

	// PaymentID is the memo, unless the merchant set its own.
	PaymentID id.PaymentID

	// Amount and PayTo are as the decision judged them, read back from the
	// payment. The requirement must agree with both.
	Amount currency.Amount
	PayTo  string

	// Requirement is the merchant's PAYMENT-REQUIRED, as the agent sent it.
	Requirement []byte
}

// SignedTransfer is a payment ready to hand to a merchant.
type SignedTransfer struct {
	Memo          string
	WalletAddress string

	// Transaction is base64, signed by the wallet.
	Transaction string

	// PaymentSignature is the value of the PAYMENT-SIGNATURE header.
	PaymentSignature string

	// LastValidBlockHeight is the height after which it can never land.
	LastValidBlockHeight uint64
}

// Quote reads what a requirement asks for, before anything is decided.
func (s *Service) Quote(requirement []byte) (Requirement, error) {
	return s.token.ParseRequirement(requirement)
}

// Transfer builds and signs an approved payment (Task 7.6). It is called after
// the decision commits, since the custodian is a network call, and it sends
// nothing: the merchant's facilitator pays the fee and submits, so the account
// wallet never holds SOL.
func (s *Service) Transfer(ctx context.Context, r TransferRequest) (SignedTransfer, error) {
	req, err := s.token.ParseRequirement(r.Requirement)
	if err != nil {
		return SignedTransfer{}, err
	}
	if !req.Amount.Equal(r.Amount) || req.PayTo != r.PayTo {
		return SignedTransfer{}, ErrRequirementDiffers
	}

	wallet, err := s.repo.Wallet(ctx, r.AccountID, s.wallets.Chain())
	if err != nil {
		return SignedTransfer{}, err
	}
	if req.FeePayer == wallet.Address {
		return SignedTransfer{}, ErrFeePayerIsWallet
	}

	memo := req.Memo
	if memo == "" {
		memo = string(r.PaymentID)
	}

	signed, err := s.rail.Transfer(ctx, RailTransfer{
		Owner:        wallet.Address,
		FeePayer:     req.FeePayer,
		Network:      s.token.Network,
		Mint:         s.token.Mint,
		PayTo:        req.PayTo,
		AtomicAmount: req.AtomicAmount,
		Memo:         memo,
		Sign: func(ctx context.Context, tx []byte) ([]byte, error) {
			return s.SignTransaction(ctx, r.AccountID, tx)
		},
	})
	if err != nil {
		return SignedTransfer{}, err
	}

	header, err := paymentSignature(req, signed.Transaction)
	if err != nil {
		return SignedTransfer{}, err
	}
	return SignedTransfer{
		Memo:                 memo,
		WalletAddress:        wallet.Address,
		Transaction:          signed.Transaction,
		PaymentSignature:     header,
		LastValidBlockHeight: signed.LastValidBlockHeight,
	}, nil
}

// paymentSignature encodes an x402 v2 payment payload the way the HTTP transport
// carries it: base64 of the JSON, with the merchant's own option and resource
// echoed byte for byte.
func paymentSignature(req Requirement, transaction string) (string, error) {
	payload := struct {
		X402Version int               `json:"x402Version"`
		Payload     map[string]string `json:"payload"`
		Accepted    json.RawMessage   `json:"accepted"`
		Resource    json.RawMessage   `json:"resource,omitempty"`
	}{
		X402Version: x402Version,
		Payload:     map[string]string{"transaction": transaction},
		Accepted:    req.Accepted,
		Resource:    req.Resource,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", apperr.Internal("settlement: encode payment payload", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
