package decisionlog

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCursor_RoundTripsToTheMicrosecond(t *testing.T) {
	c := Cursor{
		EvaluatedAt: time.Date(2026, 11, 4, 9, 30, 0, 123456000, time.UTC),
		ID:          "0b6f5c43-6a0e-4d1f-9a43-2b1c8f3f0e11",
	}

	got, err := ParseCursor(c.String())

	require.NoError(t, err)
	assert.True(t, c.EvaluatedAt.Equal(got.EvaluatedAt))
	assert.Equal(t, c.ID, got.ID)
}

func TestParseCursor_RefusesWhatThisAPIDidNotIssue(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	for name, s := range map[string]string{
		"empty":           "",
		"not base64":      "%%%",
		"no separator":    enc("1762248600000000"),
		"instant not int": enc("noon_0b6f5c43-6a0e-4d1f-9a43-2b1c8f3f0e11"),
		"id not a uuid":   enc("1762248600000000_robert'); DROP TABLE decision;--"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCursor(s)
			assert.ErrorIs(t, err, ErrInvalidCursor)
		})
	}
}

func TestQuery_ValidateBoundsThePage(t *testing.T) {
	assert.NoError(t, Query{}.Validate(), "zero means the default page")
	assert.NoError(t, Query{Outcome: OutcomeDeclined, Limit: MaxPageSize}.Validate())

	assert.Error(t, Query{Limit: -1}.Validate())
	assert.Error(t, Query{Limit: MaxPageSize + 1}.Validate())
	assert.Error(t, Query{Outcome: "held"}.Validate(), "the table holds no other outcome")
}
