// Package cdptest is a fake Coinbase Developer Platform, for tests in any
// package that holds a wallet. It speaks CDP's REST API, so the real client
// runs against it, and it signs with real keys it keeps per account, so a
// signature it returns verifies like one from CDP's enclave.
package cdptest

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
	"strings"
	"sync"
	"testing"

	sol "github.com/gagliardetto/solana-go"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
)

// Fake is a running fake CDP.
type Fake struct {
	t      *testing.T
	server *httptest.Server
	config config.CDP

	mu        sync.Mutex
	byName    map[string]sol.PrivateKey
	byAddress map[string]sol.PrivateKey
	creates   int
	signs     int
	failSigns bool
}

// New starts a fake with well-formed credentials, so the SDK can sign its
// JWTs, stopped when the test ends.
func New(t *testing.T) *Fake {
	t.Helper()
	_, apiKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("cdptest: api key: %v", err)
	}
	walletKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("cdptest: wallet key: %v", err)
	}
	walletDER, err := x509.MarshalPKCS8PrivateKey(walletKey)
	if err != nil {
		t.Fatalf("cdptest: wallet key: %v", err)
	}

	f := &Fake{t: t, byName: map[string]sol.PrivateKey{}, byAddress: map[string]sol.PrivateKey{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	f.config = config.CDP{
		APIKeyID:     "test-key",
		APIKeySecret: base64.StdEncoding.EncodeToString(apiKey),
		WalletSecret: base64.StdEncoding.EncodeToString(walletDER),
		BaseURL:      f.server.URL,
	}
	return f
}

// Config points an application at this fake.
func (f *Fake) Config() config.CDP { return f.config }

// Creates is how many create calls succeeded in making an account.
func (f *Fake) Creates() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

// Signs is how many transactions were signed.
func (f *Fake) Signs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signs
}

// FailSigning makes every signing call fail, or stops it.
func (f *Fake) FailSigning(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSigns = fail
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	const accounts = "/v2/solana/accounts"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == accounts:
		f.create(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, accounts+"/by-name/"):
		key, ok := f.byName[strings.TrimPrefix(r.URL.Path, accounts+"/by-name/")]
		if !ok {
			reply(w, http.StatusNotFound, failure("not_found"))
			return
		}
		reply(w, http.StatusOK, map[string]any{"address": key.PublicKey().String()})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sign/transaction"):
		f.sign(w, r, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, accounts+"/"), "/sign/transaction"))
	default:
		http.NotFound(w, r)
	}
}

func (f *Fake) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, http.StatusBadRequest, failure("malformed_request"))
		return
	}
	if _, exists := f.byName[body.Name]; exists {
		reply(w, http.StatusConflict, failure("already_exists"))
		return
	}
	key, err := sol.NewRandomPrivateKey()
	if err != nil {
		reply(w, http.StatusInternalServerError, failure("internal_server_error"))
		return
	}
	f.byName[body.Name], f.byAddress[key.PublicKey().String()] = key, key
	f.creates++
	reply(w, http.StatusCreated, map[string]any{"address": key.PublicKey().String(), "name": body.Name})
}

// sign adds the account's signature in its own slot and changes nothing else,
// which is what CDP does.
func (f *Fake) sign(w http.ResponseWriter, r *http.Request, address string) {
	if f.failSigns {
		reply(w, http.StatusServiceUnavailable, failure("service_unavailable"))
		return
	}
	key, ok := f.byAddress[address]
	if !ok {
		reply(w, http.StatusNotFound, failure("not_found"))
		return
	}
	var body struct {
		Transaction string `json:"transaction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(w, http.StatusBadRequest, failure("malformed_request"))
		return
	}
	raw, err := base64.StdEncoding.DecodeString(body.Transaction)
	if err != nil {
		reply(w, http.StatusBadRequest, failure("malformed_request"))
		return
	}
	tx, err := sol.TransactionFromBytes(raw)
	if err != nil {
		reply(w, http.StatusBadRequest, failure("malformed_transaction"))
		return
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		reply(w, http.StatusBadRequest, failure("malformed_transaction"))
		return
	}
	signature, err := key.Sign(message)
	if err != nil {
		reply(w, http.StatusInternalServerError, failure("internal_server_error"))
		return
	}
	required := int(tx.Message.Header.NumRequiredSignatures)
	for len(tx.Signatures) < required {
		tx.Signatures = append(tx.Signatures, sol.Signature{})
	}
	signed := false
	for i := range required {
		if tx.Message.AccountKeys[i].Equals(key.PublicKey()) {
			tx.Signatures[i] = signature
			signed = true
		}
	}
	if !signed {
		reply(w, http.StatusBadRequest, failure("not_a_signer"))
		return
	}
	out, err := tx.MarshalBinary()
	if err != nil {
		reply(w, http.StatusInternalServerError, failure("internal_server_error"))
		return
	}
	f.signs++
	reply(w, http.StatusOK, map[string]any{"signedTransaction": base64.StdEncoding.EncodeToString(out)})
}

func failure(errorType string) map[string]any {
	return map[string]any{"errorType": errorType, "errorMessage": errorType}
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
