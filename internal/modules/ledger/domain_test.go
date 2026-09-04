package ledger_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// usd is a helper for building test amounts without repeating error handling.
func usd(t *testing.T, s string) currency.Amount {
	t.Helper()
	a, err := currency.Parse(s, currency.USDC)
	require.NoError(t, err)
	return a
}

const (
	bucketA = id.LedgerAccountID("11111111-1111-1111-1111-111111111111")
	bucketB = id.LedgerAccountID("22222222-2222-2222-2222-222222222222")
)

func TestBalanced_TwoOppositeLinesSumToZero(t *testing.T) {
	lines := ledger.Transfer(bucketA, bucketB, usd(t, "0.40"))

	ok, err := ledger.Balanced(currency.USDC, lines)
	require.NoError(t, err)
	assert.True(t, ok, "a debit and an equal credit must cancel out")
}

func TestBalanced_RejectsLinesThatDoNotCancel(t *testing.T) {
	lines := []ledger.Line{
		ledger.Debit(bucketA, usd(t, "0.40")),
		ledger.Credit(bucketB, usd(t, "0.30")),
	}

	ok, err := ledger.Balanced(currency.USDC, lines)
	require.NoError(t, err)
	assert.False(t, ok, "lines that do not cancel must not be reported balanced")
}

func TestBalanced_ExactWithFractionsThatBreakBinaryFloats(t *testing.T) {
	// 0.1 and 0.2 cannot be represented exactly in binary floating point, so
	// this is the case a float-backed amount would get wrong.
	lines := []ledger.Line{
		ledger.Debit(bucketA, usd(t, "0.1")),
		ledger.Debit(bucketA, usd(t, "0.2")),
		ledger.Credit(bucketB, usd(t, "0.3")),
	}

	ok, err := ledger.Balanced(currency.USDC, lines)
	require.NoError(t, err)
	assert.True(t, ok, "0.1 + 0.2 must cancel 0.3 exactly")
}

func TestBalanced_MultiLineEntry(t *testing.T) {
	// One debit split across two credits, for example a payment that also
	// takes a fee.
	lines := []ledger.Line{
		ledger.Debit(bucketA, usd(t, "1.00")),
		ledger.Credit(bucketB, usd(t, "0.95")),
		ledger.Credit(bucketA, usd(t, "0.05")),
	}

	ok, err := ledger.Balanced(currency.USDC, lines)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestTransfer_ProducesOneDebitAndOneCredit(t *testing.T) {
	lines := ledger.Transfer(bucketA, bucketB, usd(t, "5"))

	require.Len(t, lines, 2)
	assert.True(t, lines[0].Amount.IsNegative(), "the source line must be a debit")
	assert.False(t, lines[1].Amount.IsNegative(), "the destination line must be a credit")
}

func TestEntryValidate(t *testing.T) {
	valid := func() ledger.Entry {
		return ledger.Entry{
			Kind:       ledger.KindPayment,
			Asset:      currency.USDC,
			Lines:      ledger.Transfer(bucketA, bucketB, usd(t, "0.40")),
			OccurredAt: time.Now(),
		}
	}

	cases := []struct {
		name    string
		mutate  func(*ledger.Entry)
		wantErr bool
	}{
		{
			name:    "a balanced payment is accepted",
			mutate:  func(*ledger.Entry) {},
			wantErr: false,
		},
		{
			name:    "an unknown kind is rejected",
			mutate:  func(e *ledger.Entry) { e.Kind = "nonsense" },
			wantErr: true,
		},
		{
			name:    "a missing asset is rejected",
			mutate:  func(e *ledger.Entry) { e.Asset = "" },
			wantErr: true,
		},
		{
			name:    "a single line cannot balance and is rejected",
			mutate:  func(e *ledger.Entry) { e.Lines = e.Lines[:1] },
			wantErr: true,
		},
		{
			name:    "no lines at all is rejected",
			mutate:  func(e *ledger.Entry) { e.Lines = nil },
			wantErr: true,
		},
		{
			name: "lines that do not sum to zero are rejected",
			mutate: func(e *ledger.Entry) {
				e.Lines = []ledger.Line{
					ledger.Debit(bucketA, usd(t, "0.40")),
					ledger.Credit(bucketB, usd(t, "0.30")),
				}
			},
			wantErr: true,
		},
		{
			name: "a zero amount line is rejected",
			mutate: func(e *ledger.Entry) {
				e.Lines = []ledger.Line{
					ledger.Debit(bucketA, usd(t, "0")),
					ledger.Credit(bucketB, usd(t, "0")),
				}
			},
			wantErr: true,
		},
		{
			name: "a line with no bucket is rejected",
			mutate: func(e *ledger.Entry) {
				e.Lines[0].LedgerAccountID = ""
			},
			wantErr: true,
		},
		{
			name: "a line in a different asset than the entry is rejected",
			mutate: func(e *ledger.Entry) {
				other, err := currency.Parse("0.40", currency.Asset("EURC"))
				require.NoError(t, err)
				e.Lines[0].Amount = other
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := valid()
			tc.mutate(&e)

			err := e.Validate()
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestPeriods_AreComputedInUTC(t *testing.T) {
	// A late-evening instant in a positive offset zone falls on the next UTC
	// day. Anchoring windows to UTC is what keeps two instances in different
	// regions agreeing on which window a payment belongs to.
	zone := time.FixedZone("UTC+5", 5*60*60)
	at := time.Date(2026, 9, 4, 2, 30, 0, 0, zone) // 2026-09-03 21:30 UTC

	day := ledger.DayPeriod(at)
	assert.Equal(t, ledger.PeriodDay, day.Type)
	assert.Equal(t, "2026-09-03", day.Start.Format("2006-01-02"))

	month := ledger.MonthPeriod(at)
	assert.Equal(t, ledger.PeriodMonth, month.Type)
	assert.Equal(t, "2026-09-01", month.Start.Format("2006-01-02"))
}

func TestPeriodEnd(t *testing.T) {
	at := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)

	assert.Equal(t, "2026-02-01", ledger.DayPeriod(at).End().Format("2006-01-02"))
	// January rolls into February regardless of month length.
	assert.Equal(t, "2026-02-01", ledger.MonthPeriod(at).End().Format("2006-01-02"))
}

func TestPeriodsFor_CoversDayAndMonth(t *testing.T) {
	periods := ledger.PeriodsFor(time.Now())

	require.Len(t, periods, 2, "a payment counts against both a daily and a monthly cap")
	assert.Equal(t, ledger.PeriodDay, periods[0].Type)
	assert.Equal(t, ledger.PeriodMonth, periods[1].Type)
}
