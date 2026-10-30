package mandate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// PayloadFormat versions the payload encoding itself, separately from a
// mandate's own version. A later format (adding a review threshold, say)
// changes what the bytes can say; it never changes how an existing mandate's
// bytes are read.
const PayloadFormat = 1

// payloadV1 is the encoding of a mandate's rules.
//
// Fields are declared in lexicographic key order, amounts are decimal
// strings, and every string is held to an alphabet that needs no JSON escape
// (categories are slugs, payees printable ASCII, the rest generated here).
// Under those conditions encoding/json with HTML escaping off emits exactly
// what RFC 8785 (JCS) would, which is the canonical form Task 3.1 signs. That
// equivalence is a property of the restricted input, so loosening a
// validation rule in domain.go needs a real JCS encoder here first.
//
// The agent and version are inside the bytes so an approval for one agent's
// version n cannot be filed as another agent's, or as a later version of the
// same agent's. The database refuses the mismatch as well.
type payloadV1 struct {
	AgentID     string   `json:"agent_id"`
	Asset       string   `json:"asset"`
	Categories  []string `json:"categories"`
	ExpiresAt   string   `json:"expires_at"`
	Format      int      `json:"format"`
	MaxPerDay   string   `json:"max_per_day"`
	MaxPerMonth string   `json:"max_per_month"`
	MaxPerTx    string   `json:"max_per_tx"`
	PayTo       []string `json:"pay_to"`
	Version     int      `json:"version"`
}

// Canonicalise encodes rules for one agent and version as the bytes that are
// stored, and that a signature will later cover.
//
// It is called exactly once per mandate, at issue. Everything afterwards reads
// the stored bytes, so encoding the same rules again later, with a newer build
// that orders or formats something differently, can never invalidate what was
// approved.
func Canonicalise(agentID id.AgentID, version int, r Rules) ([]byte, error) {
	r = r.normalised()

	p := payloadV1{
		AgentID:     string(agentID),
		Asset:       string(r.Asset),
		Categories:  r.Categories,
		ExpiresAt:   r.ExpiresAt.Format(time.RFC3339),
		Format:      PayloadFormat,
		MaxPerDay:   r.MaxPerDay.String(),
		MaxPerMonth: r.MaxPerMonth.String(),
		MaxPerTx:    r.MaxPerTx.String(),
		PayTo:       r.PayTo,
		Version:     version,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// JCS does not escape <, > or &. Payees are printable ASCII, so without
	// this an address containing one would encode differently from JCS.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, fmt.Errorf("mandate: encode payload: %w", err)
	}

	// Encode terminates with a newline, which is not part of the value.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// parsePayload reads stored bytes back into the agent, version and rules
// they grant.
//
// Strict on purpose: an unknown field or format is refused rather than
// skipped. A field this build does not understand may be a limit it would
// then fail to enforce, and enforcing part of what was approved is granting
// more than was approved.
func parsePayload(b []byte) (id.AgentID, int, Rules, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()

	var p payloadV1
	if err := dec.Decode(&p); err != nil {
		return "", 0, Rules{}, fmt.Errorf("mandate: decode payload: %w", err)
	}
	if dec.More() {
		return "", 0, Rules{}, fmt.Errorf("mandate: payload has trailing data")
	}
	if p.Format != PayloadFormat {
		return "", 0, Rules{}, fmt.Errorf("mandate: unsupported payload format %d", p.Format)
	}

	asset := currency.Asset(p.Asset)
	r := Rules{Asset: asset, Categories: p.Categories, PayTo: p.PayTo}

	var err error
	if r.MaxPerTx, err = currency.Parse(p.MaxPerTx, asset); err != nil {
		return "", 0, Rules{}, fmt.Errorf("mandate: max_per_tx: %w", err)
	}
	if r.MaxPerDay, err = currency.Parse(p.MaxPerDay, asset); err != nil {
		return "", 0, Rules{}, fmt.Errorf("mandate: max_per_day: %w", err)
	}
	if r.MaxPerMonth, err = currency.Parse(p.MaxPerMonth, asset); err != nil {
		return "", 0, Rules{}, fmt.Errorf("mandate: max_per_month: %w", err)
	}
	if r.ExpiresAt, err = time.Parse(time.RFC3339, p.ExpiresAt); err != nil {
		return "", 0, Rules{}, fmt.Errorf("mandate: expires_at: %w", err)
	}
	if r.PayTo == nil {
		r.PayTo = []string{}
	}

	return id.AgentID(p.AgentID), p.Version, r, nil
}
