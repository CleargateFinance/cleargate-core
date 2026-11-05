package app

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// stubCDP records the names and addresses it is called with.
type stubCDP struct {
	names, signedBy []string
}

func (s *stubCDP) SolanaAccount(_ context.Context, name string) (string, error) {
	s.names = append(s.names, name)
	return "addr-of-" + name, nil
}

func (s *stubCDP) SignSolanaTransaction(_ context.Context, address string, tx []byte) ([]byte, error) {
	s.signedBy = append(s.signedBy, address)
	return tx, nil
}

// TestSettlementWallets_NamesTheCDPAccountByTheAccountID: the name is what
// keeps one CDP account per Cleargate account, so it must be the account's
// id itself, never a per-call value.
func TestSettlementWallets_NamesTheCDPAccountByTheAccountID(t *testing.T) {
	cdp := &stubCDP{}
	w := SettlementWallets{CDP: cdp}
	accountID := id.AccountID(uuid.NewString())

	address, err := w.Create(context.Background(), accountID)
	require.NoError(t, err)
	_, err = w.Sign(context.Background(), address, []byte("tx"))
	require.NoError(t, err)

	assert.Equal(t, []string{string(accountID)}, cdp.names)
	assert.Len(t, cdp.names[0], 36, "a UUID is the longest name CDP accepts")
	assert.Equal(t, []string{"addr-of-" + string(accountID)}, cdp.signedBy)
}
