package app

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
)

// TestReconcilePeriods_IncludeTheWindowsThatJustClosed: a daily check sees
// each window whole at least once, after it closes.
func TestReconcilePeriods_IncludeTheWindowsThatJustClosed(t *testing.T) {
	firstOfMonth := time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)
	lastOfOctober := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)

	assert.Equal(t, []ledger.Period{
		ledger.DayPeriod(lastOfOctober),
		ledger.MonthPeriod(lastOfOctober),
		ledger.DayPeriod(firstOfMonth),
		ledger.MonthPeriod(firstOfMonth),
	}, reconcilePeriods(firstOfMonth, 24*time.Hour), "the first run of a month checks the month that ended")

	midMonth := time.Date(2026, 11, 15, 15, 0, 0, 0, time.UTC)
	assert.Equal(t, []ledger.Period{
		ledger.DayPeriod(midMonth.Add(-24 * time.Hour)),
		ledger.MonthPeriod(midMonth),
		ledger.DayPeriod(midMonth),
	}, reconcilePeriods(midMonth, 24*time.Hour), "a window open at both instants is checked once")
}
