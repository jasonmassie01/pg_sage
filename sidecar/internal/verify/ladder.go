package verify

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The verification ladder (lab-03a) records what a change was tested on
// before production: tier 1 plans only (a HypoPG what-if, a hint's plan
// comparison, plans under SET LOCAL), tier 2 a clone rehearsal, tier 3 the
// production verdict (a canary of one), tier 4 a staggered fleet rollout.
// A record is evidence only: the executor's entry rule reads whether a
// tier passed; nothing here changes a database.

// Ladder tiers.
const (
	TierPlanOnly = 1
	TierClone    = 2
	TierCanary   = 3
	TierFleet    = 4
)

// Ladder results.
const (
	LadderPassed       = "passed"
	LadderFailed       = "failed"
	LadderInconclusive = "inconclusive"
	LadderSkipped      = "skipped"
)

// Ladder methods: how a tier was tested.
const (
	MethodLadderWhatIf    = "hypopg_what_if"
	MethodLadderHide      = "hypopg_hide"
	MethodLadderHint      = "hint_plan"
	MethodLadderSession   = "session_setting"
	MethodLadderRehearsal = "clone_rehearsal"
	MethodLadderVerdict   = "verdict"
)

// Metrics a ladder record's Predicted is a change of, in percent.
const (
	LadderMetricPlanCost      = "plan_cost"
	LadderMetricBlocksPerCall = "blocks_per_call"
)

var tierNames = map[int]string{TierPlanOnly: "plan_only", TierClone: "clone",
	TierCanary: "canary", TierFleet: "fleet"}

var ladderResults = map[string]bool{LadderPassed: true, LadderFailed: true,
	LadderInconclusive: true, LadderSkipped: true}

// SkippedTier is a tier not tested, with why ("no clone provider
// configured", "HypoPG cannot model gin indexes").
type SkippedTier struct {
	Tier   int    `json:"tier"`
	Reason string `json:"reason"`
}

// LadderRecord is one tier's test of a change. Predicted is the change the
// test measured or predicted, in percent of Metric (negative is a fall).
type LadderRecord struct {
	Tier      int            `json:"tier"`
	Name      string         `json:"name"`
	Method    string         `json:"method,omitempty"`
	Result    string         `json:"result"`
	Predicted *float64       `json:"predicted,omitempty"`
	Metric    string         `json:"metric,omitempty"`
	Evidence  map[string]any `json:"evidence,omitempty"`
	At        time.Time      `json:"at"`
	Skipped   []SkippedTier  `json:"skipped,omitempty"`
}

// TierName is a tier's stable name, or "" for an unknown tier.
func TierName(tier int) string { return tierNames[tier] }

// Passed reports a pass of the record's own tier: a known tier, the
// passed result, and no skip of that same tier (a contradictory record
// fails closed).
func (r LadderRecord) Passed() bool {
	if r.Result != LadderPassed || tierNames[r.Tier] == "" {
		return false
	}
	for _, s := range r.Skipped {
		if s.Tier == r.Tier {
			return false
		}
	}
	return true
}

// PassedTier reports whether any record passed tier.
func PassedTier(recs []LadderRecord, tier int) bool {
	for _, r := range recs {
		if r.Tier == tier && r.Passed() {
			return true
		}
	}
	return false
}

// HighestPassed is the passed record of the highest tier (the latest of
// equals), false when none passed.
func HighestPassed(recs []LadderRecord) (LadderRecord, bool) {
	var best LadderRecord
	found := false
	for _, r := range recs {
		if r.Passed() && (!found || r.Tier >= best.Tier) {
			best, found = r, true
		}
	}
	return best, found
}

// HighestPassedName is the name of the highest tier passed, or "".
func HighestPassedName(recs []LadderRecord) string {
	if r, ok := HighestPassed(recs); ok {
		return TierName(r.Tier)
	}
	return ""
}

// ParseLadder decodes a stored ladder strictly: empty, JSON null and []
// are no records; anything but an array of valid records is an error.
func ParseLadder(raw []byte) ([]LadderRecord, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var recs []LadderRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		return nil, fmt.Errorf("decode ladder: %w", err)
	}
	for i, r := range recs {
		if tierNames[r.Tier] == "" {
			return nil, fmt.Errorf("ladder record %d has unknown tier %d", i, r.Tier)
		}
		if !ladderResults[r.Result] {
			return nil, fmt.Errorf("ladder record %d has unknown result %q", i, r.Result)
		}
	}
	if len(recs) == 0 {
		return nil, nil
	}
	return recs, nil
}

// DecodeLadderValue decodes a ladder held in a decoded JSON document (a
// finding's detail["ladder"]); nil is no records.
func DecodeLadderValue(v any) ([]LadderRecord, error) {
	if v == nil {
		return nil, nil
	}
	if _, ok := v.([]any); !ok {
		return nil, fmt.Errorf("ladder is %T, want a JSON array", v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-encode ladder: %w", err)
	}
	return ParseLadder(raw)
}

// LadderBuildError is a tier whose record could not be built: skipped,
// with the error, so it never counts as a pass.
func LadderBuildError(tier int, method string, err error, at time.Time) LadderRecord {
	if err == nil {
		err = errors.New("unknown error")
	}
	return skippedRecord(tier, method, err.Error(), at)
}

func skippedRecord(tier int, method, reason string, at time.Time) LadderRecord {
	return LadderRecord{Tier: tier, Name: TierName(tier), Method: method,
		Result: LadderSkipped, At: at.UTC(),
		Skipped: []SkippedTier{{Tier: tier, Reason: reason}}}
}

func newRecord(tier int, method, result string, at time.Time) LadderRecord {
	return LadderRecord{Tier: tier, Name: TierName(tier), Method: method, Result: result,
		At: at.UTC(), Evidence: map[string]any{}}
}

// inconclusive marks r inconclusive with why as a skip of its own tier.
func inconclusive(r LadderRecord, why string) LadderRecord {
	if why == "" {
		why = "the test did not complete"
	}
	r.Result = LadderInconclusive
	r.Skipped = append(r.Skipped, SkippedTier{Tier: r.Tier, Reason: why})
	return r
}
