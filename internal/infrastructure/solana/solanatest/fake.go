// Package solanatest is a fake Solana JSON-RPC node, for tests in any package
// that reaches the chain. It speaks the wire format, so the real client runs
// against it, and it keeps only what a test tells it.
package solanatest

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	sol "github.com/gagliardetto/solana-go"
)

// DevnetUSDC is the USDC mint on Solana devnet.
const DevnetUSDC = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"

// Fake is a running fake node.
type Fake struct {
	t      *testing.T
	server *httptest.Server

	mu           sync.Mutex
	height       uint64
	lastValid    uint64
	blockhash    sol.Hash
	mints        map[string]uint8
	signatures   map[string][]signatureEntry
	transactions map[string]transactionEntry
	calls        map[string]int
	commitments  map[string]bool
	forgotten    map[string]transactionEntry
	failing      map[string]bool
}

type signatureEntry struct {
	signature string
	failed    bool
	memo      string
	blockTime time.Time
}

type transactionEntry struct {
	raw       []byte
	failed    bool
	pre, post []tokenBalance
}

type tokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Owner         string `json:"owner"`
	Mint          string `json:"mint"`
	UITokenAmount struct {
		Amount   string `json:"amount"`
		Decimals uint8  `json:"decimals"`
	} `json:"uiTokenAmount"`
}

// New starts a fake with the devnet USDC mint at 6 decimals, height 1000 and a
// blockhash valid to 1150, stopped when the test ends.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{
		t:            t,
		height:       1000,
		lastValid:    1150,
		blockhash:    sol.HashFromBytes(bytes.Repeat([]byte{7}, 32)),
		mints:        map[string]uint8{DevnetUSDC: 6},
		signatures:   map[string][]signatureEntry{},
		transactions: map[string]transactionEntry{},
		calls:        map[string]int{},
		commitments:  map[string]bool{},
		forgotten:    map[string]transactionEntry{},
		failing:      map[string]bool{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}

// URL is the node's JSON-RPC endpoint.
func (f *Fake) URL() string { return f.server.URL }

// SetHeight sets the finalized block height.
func (f *Fake) SetHeight(height uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.height = height
}

// SetLastValid sets the last valid height of the latest blockhash.
func (f *Fake) SetLastValid(height uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastValid = height
}

// Calls is how many times method was called.
func (f *Fake) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// OnlyFinalized reports whether every call that named a commitment named
// finalized.
func (f *Fake) OnlyFinalized() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for commitment := range f.commitments {
		if commitment != "finalized" {
			return false
		}
	}
	return true
}

// Fail makes every call to method answer with an error, or stops that.
func (f *Fake) Fail(method string, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[method] = fail
}

// Forget makes the node stop returning a transaction it still lists, as a
// lagging node behind a load balancer does, until Restore.
func (f *Fake) Forget(signature string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten[signature] = f.transactions[signature]
	delete(f.transactions, signature)
}

// Restore brings back everything Forget hid.
func (f *Fake) Restore() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for signature, e := range f.forgotten {
		f.transactions[signature] = e
		delete(f.forgotten, signature)
	}
}

// RandomAddress is a fresh address nobody holds a key to.
func RandomAddress(t *testing.T) string {
	t.Helper()
	key, err := sol.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("solanatest: random key: %v", err)
	}
	return key.PublicKey().String()
}

// randomSignature is a signature that names nothing.
func randomSignature(t *testing.T) string {
	t.Helper()
	var sig sol.Signature
	if _, err := rand.Read(sig[:]); err != nil {
		t.Fatalf("solanatest: random signature: %v", err)
	}
	return sig.String()
}

// emptyTransaction is a transaction with a memo program call, so a test that
// only reads token balances has something well formed to decode.
func emptyTransaction(t *testing.T, payer string) []byte {
	t.Helper()
	key := sol.MustPublicKeyFromBase58(payer)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("solanatest: nonce: %v", err)
	}
	tx, err := sol.NewTransaction(
		[]sol.Instruction{sol.NewInstruction(sol.MemoProgramID, sol.AccountMetaSlice{}, nonce[:])},
		sol.Hash{1}, sol.TransactionPayer(key))
	if err != nil {
		t.Fatalf("solanatest: build transaction: %v", err)
	}
	tx.Signatures = make([]sol.Signature, tx.Message.Header.NumRequiredSignatures)
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("solanatest: encode transaction: %v", err)
	}
	return raw
}

func balance(owner, mint string, amount uint64) tokenBalance {
	b := tokenBalance{Owner: owner, Mint: mint}
	b.UITokenAmount.Amount = strconv.FormatUint(amount, 10)
	b.UITokenAmount.Decimals = 6
	return b
}

// Holdings is a token balance a test transaction changes.
type Holdings struct {
	Owner, Mint string
	Before      uint64
	After       uint64
}

// Transfer adds a finalized transaction that changes token balances, listed
// under each holding's associated token account, and returns its signature.
func (f *Fake) Transfer(failed bool, holdings ...Holdings) string {
	f.t.Helper()
	sig := randomSignature(f.t)
	entry := transactionEntry{failed: failed, raw: emptyTransaction(f.t, holdings[0].Owner)}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range holdings {
		entry.pre = append(entry.pre, balance(h.Owner, h.Mint, h.Before))
		entry.post = append(entry.post, balance(h.Owner, h.Mint, h.After))
		ata, _, err := sol.FindAssociatedTokenAddress(
			sol.MustPublicKeyFromBase58(h.Owner), sol.MustPublicKeyFromBase58(h.Mint))
		if err != nil {
			f.t.Fatalf("solanatest: token account: %v", err)
		}
		f.prepend(ata.String(), signatureEntry{signature: sig, failed: failed, blockTime: time.Now()})
	}
	f.transactions[sig] = entry
	return sig
}

// Land adds signedTx as a finalized transaction, listed under address with a
// memo rendered the way a node renders one, and returns its signature. The
// signature is random because in a real payment the facilitator's signature is
// the transaction's id and Cleargate never sees it.
func (f *Fake) Land(address, signedTx, memo string, failed bool, at time.Time) string {
	f.t.Helper()
	raw, err := base64.StdEncoding.DecodeString(signedTx)
	if err != nil {
		f.t.Fatalf("solanatest: decode transaction: %v", err)
	}
	sig := randomSignature(f.t)
	f.mu.Lock()
	defer f.mu.Unlock()
	rendered := ""
	if memo != "" {
		rendered = fmt.Sprintf("[%d] %s", len(memo), memo)
	}
	f.prepend(address, signatureEntry{signature: sig, failed: failed, memo: rendered, blockTime: at})
	f.transactions[sig] = transactionEntry{raw: raw, failed: failed}
	return sig
}

func (f *Fake) prepend(address string, e signatureEntry) {
	f.signatures[address] = append([]signatureEntry{e}, f.signatures[address]...)
}

type rpcRequest struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Params []json.RawMessage `json:"params"`
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[req.Method]++
	f.note(req.Params)

	result, err := f.answer(req)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": -32601, "message": err.Error()},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// note records the commitment a call asked for.
func (f *Fake) note(params []json.RawMessage) {
	for _, p := range params {
		var opts struct {
			Commitment string `json:"commitment"`
		}
		if json.Unmarshal(p, &opts) == nil && opts.Commitment != "" {
			f.commitments[opts.Commitment] = true
		}
	}
}

func (f *Fake) answer(req rpcRequest) (any, error) {
	if f.failing[req.Method] {
		return nil, fmt.Errorf("solanatest: %s is failing", req.Method)
	}
	switch req.Method {
	case "getBlockHeight":
		return f.height, nil
	case "getLatestBlockhash":
		return map[string]any{
			"context": map[string]any{"slot": f.height},
			"value":   map[string]any{"blockhash": f.blockhash.String(), "lastValidBlockHeight": f.lastValid},
		}, nil
	case "getAccountInfo":
		return f.accountInfo(req)
	case "getSignaturesForAddress":
		return f.history(req)
	case "getTransaction":
		return f.transaction(req)
	default:
		return nil, fmt.Errorf("solanatest: method %s is not faked", req.Method)
	}
}

// accountInfo answers for a mint: an SPL token Mint account, 82 bytes, with its
// decimals at offset 44 and its initialized flag at 45.
func (f *Fake) accountInfo(req rpcRequest) (any, error) {
	var address string
	if err := json.Unmarshal(req.Params[0], &address); err != nil {
		return nil, err
	}
	decimals, ok := f.mints[address]
	if !ok {
		return map[string]any{"context": map[string]any{"slot": f.height}, "value": nil}, nil
	}
	data := make([]byte, 82)
	binary.LittleEndian.PutUint64(data[36:44], 1_000_000_000)
	data[44] = decimals
	data[45] = 1
	return map[string]any{
		"context": map[string]any{"slot": f.height},
		"value": map[string]any{
			"lamports": 1, "owner": sol.TokenProgramID.String(), "executable": false, "rentEpoch": 0,
			"data": []string{base64.StdEncoding.EncodeToString(data), "base64"},
		},
	}, nil
}

func (f *Fake) history(req rpcRequest) (any, error) {
	var address string
	if err := json.Unmarshal(req.Params[0], &address); err != nil {
		return nil, err
	}
	var opts struct {
		Limit  int    `json:"limit"`
		Before string `json:"before"`
		Until  string `json:"until"`
	}
	if len(req.Params) > 1 {
		if err := json.Unmarshal(req.Params[1], &opts); err != nil {
			return nil, err
		}
	}

	out := []map[string]any{}
	started := opts.Before == ""
	for _, e := range f.signatures[address] {
		if !started {
			started = e.signature == opts.Before
			continue
		}
		if e.signature == opts.Until || (opts.Limit > 0 && len(out) == opts.Limit) {
			break
		}
		entry := map[string]any{
			"signature": e.signature, "slot": f.height, "confirmationStatus": "finalized",
			"blockTime": e.blockTime.Unix(), "err": nil, "memo": nil,
		}
		if e.failed {
			entry["err"] = map[string]any{"InstructionError": []any{0, "Custom"}}
		}
		if e.memo != "" {
			entry["memo"] = e.memo
		}
		out = append(out, entry)
	}
	return out, nil
}

func (f *Fake) transaction(req rpcRequest) (any, error) {
	var signature string
	if err := json.Unmarshal(req.Params[0], &signature); err != nil {
		return nil, err
	}
	e, ok := f.transactions[signature]
	if !ok {
		return nil, nil
	}
	meta := map[string]any{"err": nil, "preTokenBalances": e.pre, "postTokenBalances": e.post}
	if e.failed {
		meta["err"] = map[string]any{"InstructionError": []any{0, "Custom"}}
	}
	return map[string]any{
		"slot": f.height, "blockTime": time.Now().Unix(), "version": 0, "meta": meta,
		"transaction": []string{base64.StdEncoding.EncodeToString(e.raw), "base64"},
	}, nil
}
