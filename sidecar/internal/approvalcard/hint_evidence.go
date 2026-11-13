package approvalcard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// HintEvidence is the plan comparison that gated a pg_hint_plan action
// (tuning-05): the finding's detail.hint_evidence, lifted with the stored
// key names so nothing is renamed between the finding and the card. It is
// read-only and not part of the card hash.
type HintEvidence struct {
	Verdict           string          `json:"verdict"`
	Reason            string          `json:"reason,omitempty"`
	Basis             string          `json:"basis,omitempty"`
	PlainCost         *float64        `json:"plain_cost,omitempty"`
	HintedCost        *float64        `json:"hinted_cost,omitempty"`
	CostDeltaPct      *float64        `json:"cost_delta_pct,omitempty"`
	PlainPlanHash     string          `json:"plain_plan_hash,omitempty"`
	HintedPlanHash    string          `json:"hinted_plan_hash,omitempty"`
	PlainPlan         json.RawMessage `json:"plain_plan,omitempty"`
	HintedPlan        json.RawMessage `json:"hinted_plan,omitempty"`
	PlainPlanOmitted  string          `json:"plain_plan_omitted,omitempty"`
	HintedPlanOmitted string          `json:"hinted_plan_omitted,omitempty"`
	GenericPlan       bool            `json:"generic_plan"`
	PGMajor           int             `json:"pg_major,omitempty"`
}

// maxCardPlanBytes is the tuner's cap on each stored plan (64 KB): a card
// never carries a plan the writer would have left out.
const maxCardPlanBytes = 64 << 10

// hintActionTypes are the actions whose card shows the plan comparison.
var hintActionTypes = map[string]bool{"apply_query_hint": true, "retire_query_hint": true}

// hintEvidenceOf lifts the finding's hint_evidence onto a hint card. A
// malformed value gives no section and one WARN; the card still renders.
func hintEvidenceOf(in Inputs, actionType string) *HintEvidence {
	if !hintActionTypes[actionType] || in.Finding == nil {
		return nil
	}
	v, ok := in.Finding.Detail["hint_evidence"]
	if !ok || v == nil {
		return nil
	}
	h, err := parseHintEvidence(v)
	if err != nil {
		slog.Warn("approval card: finding hint_evidence is unreadable; "+
			"the card shows no plan comparison (check the finding's detail)",
			"finding_id", in.Finding.ID, "queue_id", in.Action.ID, "error", err)
		return nil
	}
	return h
}

// parseHintEvidence decodes a decoded detail value strictly: wrong types,
// a missing verdict or a plan that is not a JSON array or object fail.
func parseHintEvidence(v any) (*HintEvidence, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hint_evidence is %T, want a JSON object", v)
	}
	raw, err := compactJSON(m)
	if err != nil {
		return nil, fmt.Errorf("re-encode hint_evidence: %w", err)
	}
	var h HintEvidence
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("decode hint_evidence: %w", err)
	}
	if strings.TrimSpace(h.Verdict) == "" {
		return nil, errors.New("hint_evidence has no verdict")
	}
	if err := capPlan("plain_plan", &h.PlainPlan, &h.PlainPlanOmitted); err != nil {
		return nil, err
	}
	if err := capPlan("hinted_plan", &h.HintedPlan, &h.HintedPlanOmitted); err != nil {
		return nil, err
	}
	return &h, nil
}

// compactJSON encodes v without HTML escaping, so a plan's size is its
// compact JSON size.
func compactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// capPlan checks one plan is EXPLAIN JSON (an array or object) and drops
// a plan over maxCardPlanBytes, naming it in omitted as the writer does.
func capPlan(key string, plan *json.RawMessage, omitted *string) error {
	p := bytes.TrimSpace(*plan)
	switch {
	case len(p) == 0 || bytes.Equal(p, []byte("null")):
		*plan = nil
		return nil
	case p[0] != '[' && p[0] != '{':
		return fmt.Errorf("hint_evidence %s is not an EXPLAIN JSON array or object", key)
	case len(p) > maxCardPlanBytes:
		*plan = nil
		*omitted = fmt.Sprintf("plan of %d bytes exceeds %d", len(p), maxCardPlanBytes)
	}
	return nil
}
