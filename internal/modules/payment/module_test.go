package payment

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// TestNew_RefusesAMissingPort: every port answers a question the decision
// depends on, so none has a default.
func TestNew_RefusesAMissingPort(t *testing.T) {
	_, err := New(Deps{DB: &database.DB{}, Rail: Rail{Name: "x402-exact-solana", Token: "USDC", Network: "solana:x"}})

	assert.ErrorIs(t, err, ErrMissingDependency)
}
