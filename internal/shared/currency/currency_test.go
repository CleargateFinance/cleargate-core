package currency

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_ExactDecimal(t *testing.T) {
	a, err := Parse("0.40", USDC)
	require.NoError(t, err)
	assert.Equal(t, "0.4", a.String())
}

func TestAdd_IsExact(t *testing.T) {
	// The canonical float64 failure. This must hold exactly.
	x, err := Parse("0.1", USDC)
	require.NoError(t, err)
	y, err := Parse("0.2", USDC)
	require.NoError(t, err)

	sum, err := x.Add(y)
	require.NoError(t, err)

	want, err := Parse("0.3", USDC)
	require.NoError(t, err)
	assert.True(t, sum.Equal(want), "0.1 + 0.2 = %s, want 0.3", sum)
}

func TestAdd_RejectsAssetMismatch(t *testing.T) {
	usdc, err := Parse("1", USDC)
	require.NoError(t, err)
	other, err := Parse("1", Asset("EURC"))
	require.NoError(t, err)

	_, err = usdc.Add(other)
	assert.Error(t, err, "adding across assets must fail")
}

func TestSum_ZeroForBalancedLines(t *testing.T) {
	debit, err := Parse("-0.40", USDC)
	require.NoError(t, err)
	credit, err := Parse("0.40", USDC)
	require.NoError(t, err)

	total, err := Sum(USDC, debit, credit)
	require.NoError(t, err)
	assert.True(t, total.IsZero(), "Sum = %s, want 0", total)
}
