//go:build integration

package settlement

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// newAccount writes the rows an account needs and returns its id.
func newAccount(t *testing.T, db *fixtures.TestDB, name string) id.AccountID {
	t.Helper()
	ctx := context.Background()
	var principalID, accountID string
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO account (principal_id, display_name) VALUES ($1, $2) RETURNING id`,
		principalID, name).Scan(&accountID))
	return id.AccountID(accountID)
}

// countingProvider is idempotent per account, as WalletProvider requires, and
// safe to call from many goroutines.
type countingProvider struct {
	mu      sync.Mutex
	address string
	creates int
}

func (p *countingProvider) Chain() Chain { return ChainSolana }
func (p *countingProvider) Name() string { return "cdp" }

func (p *countingProvider) Create(context.Context, id.AccountID) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	return p.address, nil
}

func (p *countingProvider) Sign(_ context.Context, _ string, tx []byte) ([]byte, error) {
	return tx, nil
}

func TestAddress_IsRecordedOnceAndReadBack(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	accountID := newAccount(t, db, "acme")
	provider := &countingProvider{address: addrA}
	m, err := New(Deps{DB: db.DB, Wallets: provider, Accounts: everyoneIsAMember{}, Token: testToken})
	require.NoError(t, err)

	first, err := m.Service().Address(ctx, accountID)
	require.NoError(t, err)
	second, err := m.Service().Address(ctx, accountID)
	require.NoError(t, err)

	assert.Equal(t, addrA, first)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, provider.creates)

	var chain, providerName string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT chain, provider FROM account_wallet WHERE account_id = $1`, string(accountID),
	).Scan(&chain, &providerName))
	assert.Equal(t, "solana", chain)
	assert.Equal(t, "cdp", providerName)
}

// TestAddress_ConcurrentFirstCallsLeaveOneWallet: ten requests for a new
// account's address at once record one wallet and all see it.
func TestAddress_ConcurrentFirstCallsLeaveOneWallet(t *testing.T) {
	db := fixtures.NewTestDB(t)
	accountID := newAccount(t, db, "acme")
	m, err := New(Deps{DB: db.DB, Wallets: &countingProvider{address: addrA}, Accounts: everyoneIsAMember{}, Token: testToken})
	require.NoError(t, err)

	const n = 10
	got := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = m.Service().Address(context.Background(), accountID)
		}()
	}
	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, addrA, got[i])
	}
	var rows int
	require.NoError(t, db.QueryRow(context.Background(),
		`SELECT count(*) FROM account_wallet WHERE account_id = $1`, string(accountID)).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func TestSave_AnAddressAlreadyAnotherAccountsFails(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	repo := NewPostgresRepository(db.DB)
	acme, globex := newAccount(t, db, "acme"), newAccount(t, db, "globex")

	_, err := repo.Save(ctx, Wallet{AccountID: acme, Chain: ChainSolana, Provider: "cdp", Address: addrA})
	require.NoError(t, err)
	_, err = repo.Save(ctx, Wallet{AccountID: globex, Chain: ChainSolana, Provider: "cdp", Address: addrA})

	// Refused by the address's unique index, not skipped as if globex already
	// had a wallet: a deposit to one address must never credit two accounts.
	assert.ErrorContains(t, err, "account_wallet_address_key")
	_, err = repo.Wallet(ctx, globex, ChainSolana)
	assert.ErrorIs(t, err, ErrNoWallet)
}

// The tests below write rows directly, the way a buggy script or an operator
// would, to show what the schema refuses on its own.

func TestAccountWallet_OnePerAccountPerChain(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	accountID := newAccount(t, db, "acme")
	insert := `INSERT INTO account_wallet (account_id, chain, provider, address) VALUES ($1, 'solana', 'cdp', $2)`

	_, err := db.Exec(ctx, insert, string(accountID), addrA)
	require.NoError(t, err)
	_, err = db.Exec(ctx, insert, string(accountID), addrB)

	assert.Error(t, err, "an account's money is pooled in one wallet, never one per agent")
}

func TestAccountWallet_RefusesWhatIsNotASolanaAddress(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	accountID := newAccount(t, db, "acme")

	for name, address := range map[string]string{
		"an EVM address":      "0x52908400098527886E0F7030069857D2E4169EE7",
		"base58's missing 0":  "0WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM",
		"too short":           "9WzDXwBbmkg8ZTbNMqUx",
		"empty":               "",
		"padded with a space": " " + addrA,
	} {
		_, err := db.Exec(ctx,
			`INSERT INTO account_wallet (account_id, chain, provider, address) VALUES ($1, 'solana', 'cdp', $2)`,
			string(accountID), address)
		assert.Error(t, err, name)
	}
}

func TestAccountWallet_IsNeverChangedOrDeleted(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	accountID := newAccount(t, db, "acme")
	_, err := db.Exec(ctx,
		`INSERT INTO account_wallet (account_id, chain, provider, address) VALUES ($1, 'solana', 'cdp', $2)`,
		string(accountID), addrA)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `UPDATE account_wallet SET address = $1 WHERE account_id = $2`, addrB, string(accountID))
	assert.ErrorContains(t, err, "append-only")
	_, err = db.Exec(ctx, `DELETE FROM account_wallet WHERE account_id = $1`, string(accountID))
	assert.ErrorContains(t, err, "append-only")
}

func TestAccountWallet_BelongsToARealAccount(t *testing.T) {
	db := fixtures.NewTestDB(t)

	_, err := db.Exec(context.Background(),
		`INSERT INTO account_wallet (account_id, chain, provider, address) VALUES (gen_random_uuid(), 'solana', 'cdp', $1)`,
		addrA)

	assert.Error(t, err)
}
