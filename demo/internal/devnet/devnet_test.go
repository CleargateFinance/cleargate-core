package devnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const wallet = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"

// fakeCDP answers the faucet route with reply, after checking the request.
func fakeCDP(t *testing.T, reply func(w http.ResponseWriter)) *Faucet {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST /v2/solana/faucet", r.Method+" "+r.URL.Path)
		assert.NotEmpty(t, r.Header.Get("Authorization"), "the API key's JWT")
		assert.Empty(t, r.Header.Get("X-Wallet-Auth"), "the faucet moves CDP's funds, so no wallet secret")
		var body struct{ Address, Token string }
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, wallet, body.Address)
		assert.Equal(t, "usdc", body.Token)
		w.Header().Set("Content-Type", "application/json")
		reply(w)
	}))
	t.Cleanup(srv.Close)

	// A well-formed key, so the SDK can sign its JWT.
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	f, err := NewFaucet("test-key", base64.StdEncoding.EncodeToString(key), srv.URL)
	require.NoError(t, err)
	return f
}

func TestNewFaucet_RefusesAMissingKey(t *testing.T) {
	_, err := NewFaucet("", "secret", "")
	assert.ErrorIs(t, err, ErrNoCDPKey)

	_, err = NewFaucet("id", "", "")
	assert.ErrorIs(t, err, ErrNoCDPKey)
}

func TestSendUSDC_ReturnsTheTransactionSignature(t *testing.T) {
	f := fakeCDP(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"transactionSignature":"5xFaucet"}`))
	})

	got, err := f.SendUSDC(t.Context(), wallet)

	require.NoError(t, err)
	assert.Equal(t, "5xFaucet", got)
}

// TestSendUSDC_TheDailyLimitIsItsOwnError: the console falls back to Circle's
// faucet on it, rather than failing.
func TestSendUSDC_TheDailyLimitIsItsOwnError(t *testing.T) {
	f := fakeCDP(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errorType":"faucet_limit_exceeded","errorMessage":"limit reached"}`))
	})

	_, err := f.SendUSDC(t.Context(), wallet)

	assert.ErrorIs(t, err, ErrFaucetLimit)
}

func TestSendUSDC_ARefusalNamesCDPsErrorAndCorrelationID(t *testing.T) {
	f := fakeCDP(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errorType":"invalid_request","errorMessage":"bad address","correlationId":"corr-7"}`))
	})

	_, err := f.SendUSDC(t.Context(), wallet)

	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrFaucetLimit)
	assert.ErrorContains(t, err, "invalid_request: bad address")
	assert.ErrorContains(t, err, "corr-7")
}

// fakeNode answers getAccountInfo with value, or with a JSON-RPC error when
// value is nil and fail is set.
func fakeNode(t *testing.T, value any, fail bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "getAccountInfo", req.Method)
		answer := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if fail {
			answer["error"] = map[string]any{"code": -32005, "message": "node is behind"}
		} else {
			answer["result"] = map[string]any{"context": map[string]any{"slot": 1}, "value": value}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestHasUSDCAccount_AnExistingTokenAccount(t *testing.T) {
	node := fakeNode(t, map[string]any{
		"data": []string{"", "base64"}, "executable": false, "lamports": 2039280,
		"owner": "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", "rentEpoch": 0,
	}, false)

	ready, err := HasUSDCAccount(t.Context(), node, wallet)

	require.NoError(t, err)
	assert.True(t, ready)
}

func TestHasUSDCAccount_NoTokenAccountYet(t *testing.T) {
	ready, err := HasUSDCAccount(t.Context(), fakeNode(t, nil, false), wallet)

	require.NoError(t, err)
	assert.False(t, ready)
}

// TestHasUSDCAccount_ANodeErrorIsNotAMissingAccount: funding a wallet that
// already has its account wastes a faucet request, so not knowing is an error.
func TestHasUSDCAccount_ANodeErrorIsNotAMissingAccount(t *testing.T) {
	_, err := HasUSDCAccount(t.Context(), fakeNode(t, nil, true), wallet)

	assert.ErrorContains(t, err, "node is behind")
}

func TestHasUSDCAccount_RefusesWhatIsNotAnAddress(t *testing.T) {
	_, err := HasUSDCAccount(t.Context(), "http://127.0.0.1:1", "not-an-address")

	assert.ErrorContains(t, err, "not a Solana address")
}
