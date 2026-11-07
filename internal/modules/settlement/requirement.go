package settlement

import (
	"encoding/json"
	"math/big"
	"regexp"
	"unicode/utf8"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// x402Version is the only protocol version this rail pays (§8.6).
const x402Version = 2

// maxMemoBytes is the x402 limit on a merchant's memo.
const maxMemoBytes = 256

var (
	// solanaAddress is a base58 Ed25519 public key, as account_wallet checks.
	solanaAddress = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)

	// positiveInteger is what the x402 spec says an amount is: atomic units as
	// a string. A decimal point, a sign, an exponent or a leading zero is a
	// malformed amount, never something to interpret.
	positiveInteger = regexp.MustCompile(`^[1-9][0-9]*$`)

	// maxAtomicAmount is a u64, which is what the token program moves.
	maxAtomicAmount = new(big.Int).SetUint64(^uint64(0))
)

var (
	// ErrNoUsableOption is a requirement offering nothing this rail can pay:
	// not `exact`, not USDC, or not on the configured network.
	ErrNoUsableOption = apperr.Invalid("the merchant offers no exact USDC payment on this network")

	// ErrMalformedRequirement is a requirement that cannot be read as x402, or
	// whose chosen option is not well formed.
	ErrMalformedRequirement = apperr.Invalid("the payment requirement is malformed")

	// ErrRequirementDiffers is a requirement that does not match the payment it
	// is being signed for. It should be unreachable, since the idempotency hash
	// covers the amount and the payee. It is checked anyway, because this is
	// the last point at which money can be stopped.
	ErrRequirementDiffers = apperr.Conflict("the payment requirement differs from the payment that was approved")

	// ErrFeePayerIsWallet is a merchant naming the customer's own wallet as fee
	// payer, which would have that wallet pay the fees and sign as the payer of
	// everything in the transaction.
	ErrFeePayerIsWallet = apperr.Invalid("the merchant named the account's own wallet as fee payer")
)

// Requirement is the one option chosen from a merchant's PAYMENT-REQUIRED.
type Requirement struct {
	// Accepted and Resource are the merchant's own bytes, echoed back in the
	// payment payload. A merchant compares them with what it issued, so they
	// are never re-encoded.
	Accepted json.RawMessage
	Resource json.RawMessage

	// ResourceURL is resource.url, the endpoint being paid for, if it was named.
	ResourceURL string

	// AtomicAmount is the amount as the merchant wrote it, in atomic units.
	AtomicAmount string
	PayTo        string
	FeePayer     string

	// Memo is extra.memo, empty when the merchant set none.
	Memo string

	// Amount is AtomicAmount converted to the unit of account, at par.
	Amount currency.Amount
}

// Terms is what else in the chosen option makes it this payment, beyond the
// amount, payee and resource the decision already judges: whose facilitator
// submits it, and the merchant's order reference. An idempotency key retried
// with other terms is another payment, and must not get this one's transaction.
func (r Requirement) Terms() string {
	// Marshalling two strings cannot fail.
	raw, _ := json.Marshal(struct {
		FeePayer string `json:"fee_payer"`
		Memo     string `json:"memo,omitempty"`
	}{r.FeePayer, r.Memo})
	return string(raw)
}

// ParseRequirement reads a merchant's x402 PAYMENT-REQUIRED and chooses the
// option this rail can pay: `exact`, on the configured network, in the
// configured mint.
//
// Everything a merchant sends is untrusted. The decimals come from t and are
// never read from the requirement, whatever it claims in extra, so a merchant
// cannot make one token look like a thousand.
func (t Token) ParseRequirement(raw []byte) (Requirement, error) {
	var required struct {
		X402Version int               `json:"x402Version"`
		Accepts     []json.RawMessage `json:"accepts"`
		Resource    json.RawMessage   `json:"resource"`
	}
	if err := json.Unmarshal(raw, &required); err != nil {
		return Requirement{}, ErrMalformedRequirement
	}
	if required.X402Version != x402Version {
		return Requirement{}, ErrNoUsableOption
	}

	for _, accepted := range required.Accepts {
		var option struct {
			Scheme  string `json:"scheme"`
			Network string `json:"network"`
			Asset   string `json:"asset"`
			Amount  string `json:"amount"`
			PayTo   string `json:"payTo"`
			Extra   struct {
				FeePayer string `json:"feePayer"`
				Memo     string `json:"memo"`
			} `json:"extra"`
		}
		// One option we cannot read may be for a chain we have never heard of,
		// with fields in shapes we do not know. It is skipped, not fatal.
		if err := json.Unmarshal(accepted, &option); err != nil {
			continue
		}
		if option.Scheme != "exact" || option.Network != t.Network || option.Asset != t.Mint {
			continue
		}

		amount, err := t.amountFromAtomic(option.Amount)
		if err != nil {
			return Requirement{}, err
		}
		if !solanaAddress.MatchString(option.PayTo) || !solanaAddress.MatchString(option.Extra.FeePayer) {
			return Requirement{}, ErrMalformedRequirement
		}
		if len(option.Extra.Memo) > maxMemoBytes || !utf8.ValidString(option.Extra.Memo) {
			return Requirement{}, ErrMalformedRequirement
		}

		var resource struct {
			URL string `json:"url"`
		}
		// A resource that does not parse is only a missing URL: the agent's own
		// report of what it is paying for still stands.
		_ = json.Unmarshal(required.Resource, &resource)

		return Requirement{
			Accepted:     accepted,
			Resource:     required.Resource,
			ResourceURL:  resource.URL,
			AtomicAmount: option.Amount,
			PayTo:        option.PayTo,
			FeePayer:     option.Extra.FeePayer,
			Memo:         option.Extra.Memo,
			Amount:       amount,
		}, nil
	}
	return Requirement{}, ErrNoUsableOption
}

// amountFromAtomic reads an x402 amount and converts it to USD exactly.
func (t Token) amountFromAtomic(atomic string) (currency.Amount, error) {
	if !positiveInteger.MatchString(atomic) {
		return currency.Amount{}, ErrMalformedRequirement
	}
	value, ok := new(big.Int).SetString(atomic, 10)
	if !ok || value.Cmp(maxAtomicAmount) > 0 {
		return currency.Amount{}, ErrMalformedRequirement
	}
	return t.ToUSD(value)
}
