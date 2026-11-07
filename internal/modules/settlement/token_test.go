package settlement

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestToUSD_ConvertsAtomicUnitsExactly: money must survive conversion to the
// last atomic unit, at any size a uint64 can hold.
func TestToUSD_ConvertsAtomicUnitsExactly(t *testing.T) {
	for atomic, want := range map[string]string{
		"1":                    "0.000001",
		"10000":                "0.01",
		"1234567":              "1.234567",
		"1000000":              "1",
		"18446744073709551615": "18446744073709.551615",
	} {
		v, ok := new(big.Int).SetString(atomic, 10)
		require.True(t, ok)

		got, err := testToken.ToUSD(v)

		require.NoError(t, err)
		assert.Equal(t, want, got.String(), atomic)
	}
}
