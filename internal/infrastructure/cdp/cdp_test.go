package cdp

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run against a fake CDP, to pin how the client reads each
// answer. TestWallet_* in cdp_devnet_test.go runs the same calls against the
// real one.

const address = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"

// testOptions are well-formed credentials, so the SDK can sign its JWTs, for
// a client pointed at url.
func testOptions(t *testing.T, url string) Options {
	t.Helper()
	_, apiKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	walletKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	walletDER, err := x509.MarshalPKCS8PrivateKey(walletKey)
	require.NoError(t, err)
	return Options{
		APIKeyID:     "test-key",
		APIKeySecret: base64.StdEncoding.EncodeToString(apiKey),
		WalletSecret: base64.StdEncoding.EncodeToString(walletDER),
		BaseURL:      url,
	}
}

// fakeCDP answers by method and path, and records what it was asked.
type fakeCDP struct {
	t        *testing.T
	routes   map[string]func(w http.ResponseWriter, r *http.Request)
	requests []string
}

// newFakeCDP starts a fake and a client pointed at it; adjust edits the
// client's options before it is built.
func newFakeCDP(t *testing.T, adjust ...func(*Options)) (*fakeCDP, *Client) {
	f := &fakeCDP{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key)
		assert.NotEmpty(t, r.Header.Get("Authorization"), "every call carries the API key's JWT")
		if r.Method == http.MethodPost {
			assert.NotEmpty(t, r.Header.Get("X-Wallet-Auth"), "creating and signing carry the wallet secret's JWT")
		}
		route, ok := f.routes[key]
		if !ok {
			t.Errorf("unexpected call %s", key)
			http.NotFound(w, r)
			return
		}
		route(w, r)
	}))
	t.Cleanup(srv.Close)
	o := testOptions(t, srv.URL)
	for _, a := range adjust {
		a(&o)
	}
	c, err := New(o)
	require.NoError(t, err)
	return f, c
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestSolanaAccount_CreatesANamedAccount(t *testing.T) {
	f, c := newFakeCDP(t)
	f.routes["POST /v2/solana/accounts"] = func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "acct-1", body.Name)
		reply(w, http.StatusCreated, map[string]any{"address": address, "name": body.Name})
	}

	got, err := c.SolanaAccount(t.Context(), "acct-1")

	require.NoError(t, err)
	assert.Equal(t, address, got)
}

// TestSolanaAccount_ANameTakenReadsTheExistingAccount: a retry, or the loser
// of two concurrent creates, gets the account the first one made.
func TestSolanaAccount_ANameTakenReadsTheExistingAccount(t *testing.T) {
	f, c := newFakeCDP(t)
	f.routes["POST /v2/solana/accounts"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusConflict, map[string]any{"errorType": "already_exists", "errorMessage": "name taken"})
	}
	f.routes["GET /v2/solana/accounts/by-name/acct-1"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]any{"address": address, "name": "acct-1"})
	}

	got, err := c.SolanaAccount(t.Context(), "acct-1")

	require.NoError(t, err)
	assert.Equal(t, address, got)
	assert.Equal(t, []string{"POST /v2/solana/accounts", "GET /v2/solana/accounts/by-name/acct-1"}, f.requests)
}

func TestSolanaAccount_ARefusalNamesCDPsErrorAndCorrelationID(t *testing.T) {
	f, c := newFakeCDP(t)
	f.routes["POST /v2/solana/accounts"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusUnauthorized, map[string]any{
			"errorType": "unauthorized", "errorMessage": "wallet auth is invalid", "correlationId": "corr-42",
		})
	}

	_, err := c.SolanaAccount(t.Context(), "acct-1")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")
	assert.Contains(t, err.Error(), "unauthorized: wallet auth is invalid")
	assert.Contains(t, err.Error(), "corr-42")
	assert.Equal(t, []string{"POST /v2/solana/accounts"}, f.requests, "only a 409 means the account exists")
}

func TestSignSolanaTransaction_SendsAndReturnsTheTransactionBytes(t *testing.T) {
	f, c := newFakeCDP(t)
	unsigned, signed := []byte{1, 0, 0, 7}, []byte{1, 9, 9, 7}
	f.routes["POST /v2/solana/accounts/"+address+"/sign/transaction"] = func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Transaction string }
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, base64.StdEncoding.EncodeToString(unsigned), body.Transaction)
		reply(w, http.StatusOK, map[string]any{"signedTransaction": base64.StdEncoding.EncodeToString(signed)})
	}

	got, err := c.SignSolanaTransaction(t.Context(), address, unsigned)

	require.NoError(t, err)
	assert.Equal(t, signed, got)
}

func TestSignSolanaTransaction_ARefusalIsAnError(t *testing.T) {
	f, c := newFakeCDP(t)
	f.routes["POST /v2/solana/accounts/"+address+"/sign/transaction"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusForbidden, map[string]any{"errorType": "policy_violation", "errorMessage": "denied"})
	}

	_, err := c.SignSolanaTransaction(t.Context(), address, []byte{1})

	assert.ErrorContains(t, err, "policy_violation: denied")
}

func TestClient_GivesUpAfterItsTimeout(t *testing.T) {
	f, c := newFakeCDP(t, func(o *Options) { o.Timeout = 50 * time.Millisecond })
	// The handler hangs until the test ends. Cleanups run last-registered
	// first, so this releases it before the server's Close waits on it.
	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })
	f.routes["POST /v2/solana/accounts"] = func(http.ResponseWriter, *http.Request) { <-hung }

	start := time.Now()
	_, err := c.SolanaAccount(t.Context(), "acct-1")

	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestNew_RefusesMissingCredentials(t *testing.T) {
	o := testOptions(t, "")
	for name, blank := range map[string]func(*Options){
		"api key id":     func(o *Options) { o.APIKeyID = "" },
		"api key secret": func(o *Options) { o.APIKeySecret = "" },
		"wallet secret":  func(o *Options) { o.WalletSecret = "" },
	} {
		missing := o
		blank(&missing)
		_, err := New(missing)
		assert.ErrorIs(t, err, ErrMissingCredentials, name)
	}
}
