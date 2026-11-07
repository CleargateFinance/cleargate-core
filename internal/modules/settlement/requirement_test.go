package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

const (
	merchant = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"
	feePayer = "BPFLoaderUpgradeab1e11111111111111111111111"
)

func option(amount, memo string) string {
	extra := fmt.Sprintf(`{"feePayer":%q}`, feePayer)
	if memo != "" {
		extra = fmt.Sprintf(`{"feePayer":%q,"memo":%q}`, feePayer, memo)
	}
	return fmt.Sprintf(`{"scheme":"exact","network":%q,"asset":%q,"amount":%q,"payTo":%q,"maxTimeoutSeconds":60,"extra":%s}`,
		testToken.Network, testToken.Mint, amount, merchant, extra)
}

func required(options ...string) []byte {
	joined := ""
	for i, o := range options {
		if i > 0 {
			joined += ","
		}
		joined += o
	}
	return fmt.Appendf(nil, `{"x402Version":2,"resource":{"url":"https://api.example.com/v1/quote"},"accepts":[%s]}`, joined)
}

func usd(t *testing.T, s string) currency.Amount {
	t.Helper()
	a, err := currency.Parse(s, currency.USD)
	require.NoError(t, err)
	return a
}

// TestRequirement_ConvertsAtomicUnitsExactly: "10000" is 0.01 USDC, by the x402
// spec's definition of amount.
func TestRequirement_ConvertsAtomicUnitsExactly(t *testing.T) {
	for atomic, want := range map[string]string{
		"1":                    "0.000001",
		"10000":                "0.01",
		"1500000":              "1.5",
		"18446744073709551615": "18446744073709.551615",
	} {
		got, err := testToken.ParseRequirement(required(option(atomic, "")))
		require.NoError(t, err, atomic)
		assert.Equal(t, want, got.Amount.String())
		assert.Equal(t, currency.USD, got.Amount.Asset())
		assert.Equal(t, atomic, got.AtomicAmount, "what gets signed is the merchant's own integer")
	}
}

func TestRequirement_MalformedAmountIsDeclined(t *testing.T) {
	for _, amount := range []string{
		"", "0", "-1", "+1", "1.5", "1e6", "0100", " 1", "1 ", "0x10", "abc", "18446744073709551616",
	} {
		_, err := testToken.ParseRequirement(required(option(amount, "")))
		assert.ErrorIs(t, err, ErrMalformedRequirement, "amount %q", amount)
	}
}

// TestRequirement_DecimalsComeFromConfigNotMerchant: a merchant who claims the
// token has 2 decimals must not be able to make 10000 mean 100 dollars or a
// hundredth of what it costs.
func TestRequirement_DecimalsComeFromConfigNotMerchant(t *testing.T) {
	lying := fmt.Sprintf(`{"scheme":"exact","network":%q,"asset":%q,"amount":"10000","payTo":%q,"decimals":2,
		"extra":{"feePayer":%q,"decimals":2,"name":"USDC"}}`, testToken.Network, testToken.Mint, merchant, feePayer)

	got, err := testToken.ParseRequirement(required(lying))

	require.NoError(t, err)
	assert.Equal(t, "0.01", got.Amount.String())
}

func TestSign_NoSolanaUSDCOptionIsDeclined(t *testing.T) {
	wrong := func(field, value string) string {
		var o map[string]any
		require.NoError(t, json.Unmarshal([]byte(option("10000", "")), &o))
		o[field] = value
		raw, err := json.Marshal(o)
		require.NoError(t, err)
		return string(raw)
	}

	for name, body := range map[string][]byte{
		"another scheme":  required(wrong("scheme", "upto")),
		"another network": required(wrong("network", "eip155:8453")),
		"another token":   required(wrong("asset", "So11111111111111111111111111111111111111112")),
		"no options":      required(),
		"another version": []byte(`{"x402Version":1,"accepts":[` + option("10000", "") + `]}`),
	} {
		_, err := testToken.ParseRequirement(body)
		assert.ErrorIs(t, err, ErrNoUsableOption, name)
	}
}

func TestRequirement_RefusesWhatIsNotWellFormed(t *testing.T) {
	for name, body := range map[string][]byte{
		"not json":      []byte(`nope`),
		"empty":         nil,
		"bad payee":     fmt.Appendf(nil, `{"x402Version":2,"accepts":[{"scheme":"exact","network":%q,"asset":%q,"amount":"1","payTo":"nope","extra":{"feePayer":%q}}]}`, testToken.Network, testToken.Mint, feePayer),
		"no fee payer":  fmt.Appendf(nil, `{"x402Version":2,"accepts":[{"scheme":"exact","network":%q,"asset":%q,"amount":"1","payTo":%q}]}`, testToken.Network, testToken.Mint, merchant),
		"oversize memo": required(option("1", string(make([]byte, 0))+fmt.Sprintf("%0257d", 0))),
	} {
		_, err := testToken.ParseRequirement(body)
		assert.ErrorIs(t, err, ErrMalformedRequirement, name)
	}
}

// TestRequirement_ChoosesTheOptionItCanPay: a merchant offering several chains
// lists them in its own order, some in shapes this rail has never seen.
func TestRequirement_ChoosesTheOptionItCanPay(t *testing.T) {
	base := `{"scheme":"exact","network":"eip155:8453","asset":"0xabc","amount":"99","payTo":"0xdef"}`
	strange := `{"scheme":{"nested":true}}`

	got, err := testToken.ParseRequirement(required(base, strange, option("10000", "")))

	require.NoError(t, err)
	assert.Equal(t, "0.01", got.Amount.String())
	assert.Equal(t, merchant, got.PayTo)
	assert.Equal(t, feePayer, got.FeePayer)
	assert.Equal(t, "https://api.example.com/v1/quote", got.ResourceURL)
	assert.JSONEq(t, option("10000", ""), string(got.Accepted))
}

func transferFixture(wallet string) (*Service, *fakeRail) {
	rail := &fakeRail{}
	repo := &fakeRepo{wallets: map[id.AccountID]Wallet{"acct": {AccountID: "acct", Chain: ChainSolana, Address: wallet}}}
	svc := NewService(repo, &fakeProvider{})
	svc.rail, svc.token = rail, testToken
	return svc, rail
}

func transferRequest(t *testing.T, requirement []byte) TransferRequest {
	t.Helper()
	return TransferRequest{
		AccountID: "acct", PaymentID: "the-payment-id", Amount: usd(t, "0.01"), PayTo: merchant, Requirement: requirement,
	}
}

func TestSign_RefusesRequirementThatDiffersFromDecision(t *testing.T) {
	svc, rail := transferFixture(addrA)

	for name, change := range map[string]func(*TransferRequest){
		"another amount": func(r *TransferRequest) { r.Amount = usd(t, "0.02") },
		"another payee":  func(r *TransferRequest) { r.PayTo = addrB },
	} {
		t.Run(name, func(t *testing.T) {
			r := transferRequest(t, required(option("10000", "")))
			change(&r)

			_, err := svc.Transfer(context.Background(), r)

			assert.ErrorIs(t, err, ErrRequirementDiffers)
			assert.Empty(t, rail.got.Owner, "nothing reached the rail")
		})
	}
}

func TestSign_MemoIsPaymentID(t *testing.T) {
	svc, rail := transferFixture(addrA)

	got, err := svc.Transfer(context.Background(), transferRequest(t, required(option("10000", ""))))

	require.NoError(t, err)
	assert.Equal(t, "the-payment-id", got.Memo)
	assert.Equal(t, "the-payment-id", rail.got.Memo)
	assert.Equal(t, addrA, rail.got.Owner)
	assert.Equal(t, "10000", rail.got.AtomicAmount, "the merchant's own integer, not a conversion back")
	assert.Equal(t, uint64(1150), got.LastValidBlockHeight)
}

func TestSign_MerchantsMemoIsKept(t *testing.T) {
	svc, rail := transferFixture(addrA)

	got, err := svc.Transfer(context.Background(), transferRequest(t, required(option("10000", "order-7"))))

	require.NoError(t, err)
	assert.Equal(t, "order-7", got.Memo)
	assert.Equal(t, "order-7", rail.got.Memo)
}

// TestSign_RefusesTheWalletAsFeePayer: a merchant naming the customer's wallet
// as fee payer is asking it to pay for everything in the transaction.
func TestSign_RefusesTheWalletAsFeePayer(t *testing.T) {
	svc, rail := transferFixture(feePayer)

	_, err := svc.Transfer(context.Background(), transferRequest(t, required(option("10000", ""))))

	assert.ErrorIs(t, err, ErrFeePayerIsWallet)
	assert.Empty(t, rail.got.Owner)
}

// TestSign_DigestIsOfTheSignedMessage: the digest is what stops two payments
// holding one transaction, so it must be of the message the signers signed.
func TestSign_DigestIsOfTheSignedMessage(t *testing.T) {
	svc, rail := transferFixture(addrA)

	got, err := svc.Transfer(context.Background(), transferRequest(t, required(option("10000", "order-7"))))

	require.NoError(t, err)
	want := sha256.Sum256([]byte("message:order-7"))
	assert.Equal(t, want[:], got.MessageDigest)

	rail.noMessage = true
	_, err = svc.Transfer(context.Background(), transferRequest(t, required(option("10000", "order-7"))))
	assert.Error(t, err, "nothing is handed out that cannot be told apart")
}

func TestSign_ASigningFailureReachesTheCaller(t *testing.T) {
	svc, rail := transferFixture(addrA)
	rail.err = errors.New("custodian down")

	_, err := svc.Transfer(context.Background(), transferRequest(t, required(option("10000", ""))))

	assert.ErrorContains(t, err, "custodian down")
}

// TestPaymentSignature_EchoesTheMerchantsBytes: a merchant compares accepted and
// resource with what it issued, so they must survive unchanged.
func TestPaymentSignature_EchoesTheMerchantsBytes(t *testing.T) {
	svc, _ := transferFixture(addrA)
	opt := option("10000", "order-7")

	got, err := svc.Transfer(context.Background(), transferRequest(t, required(opt)))
	require.NoError(t, err)

	raw, err := base64.StdEncoding.DecodeString(got.PaymentSignature)
	require.NoError(t, err)
	var payload struct {
		X402Version int               `json:"x402Version"`
		Payload     map[string]string `json:"payload"`
		Accepted    json.RawMessage   `json:"accepted"`
		Resource    json.RawMessage   `json:"resource"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	assert.Equal(t, 2, payload.X402Version)
	assert.Equal(t, "c2lnbmVk", payload.Payload["transaction"])
	assert.Equal(t, opt, string(payload.Accepted))
	assert.JSONEq(t, `{"url":"https://api.example.com/v1/quote"}`, string(payload.Resource))
}
