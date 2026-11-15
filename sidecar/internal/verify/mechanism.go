package verify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/planhash"
)

// The mechanism gate of verification v2 (lab-01b). An index step is
// credited for a target only when the index can have caused the target's
// change: its plan fingerprint changed within one collector cycle of the
// step and its latest plan names the index (for a drop, no longer names
// it). A target that improved without the mechanism is unexplained and
// excluded. A setting's mechanism is the metric the executor maps to it.
// Pure functions over typed inputs; PostgresObservationSource reads them.

// Action classes the gate knows (the names of internal/earned's classes,
// which verify cannot import).
const (
	MechanismIndexCreate  = "index_create"
	MechanismIndexReplace = "index_replace"
	MechanismIndexDrop    = "index_drop"
	MechanismConfigGUC    = "config_guc"
	MechanismAutovacuum   = "autovacuum_tuning"
	MechanismWALBound     = "wal_bound"
)

// Gate reasons and plan bases.
const (
	ReasonNoMechanism          = "no_mechanism"
	ReasonMechanismUnavailable = "mechanism_unavailable"
	PlanBasisExecuted          = "executed plan"
	PlanBasisEstimated         = "estimated plan"
)

// Per-target reasons of the gate.
const (
	ReasonMechCounted        = "plan changed at the step and names the index"
	ReasonMechDropCounted    = "plan changed at the step and no longer names the index"
	ReasonMechNoHashes       = "no plan fingerprints"
	ReasonMechNoPlanBefore   = "no plan fingerprint at or before the step"
	ReasonMechUnchanged      = "plan unchanged since the step"
	ReasonMechChangedLate    = "plan changed more than one collector cycle after the step"
	ReasonMechNoPlan         = "no plan captured after the step"
	ReasonMechNotAPlan       = "no plan: the latest capture is not an EXPLAIN plan"
	ReasonMechIndexUnused    = "latest plan does not name the index"
	ReasonMechIndexStillUsed = "latest plan still names the dropped index"
)

// PlanSample is one query_store sample's plan fingerprint.
type PlanSample struct {
	At   time.Time
	Hash string
}

// MechanismInput is what the gate reads. PlanHashes are each target's
// fingerprints around the step; LatestPlans each target's newest plan
// captured after it. Metric is the outcome metric the executor maps to a
// setting ("" when none).
type MechanismInput struct {
	Class       string
	Index       string
	Targets     []int64
	ExecutedAt  time.Time
	Cycle       time.Duration
	PlanHashes  map[int64][]PlanSample
	LatestPlans map[int64][]byte
	Metric      string
}

// TargetMechanism is one target's result.
type TargetMechanism struct {
	QueryID   int64
	Counted   bool
	Reason    string
	PlanBasis string
}

// Mechanism is the gate's result. Outcome is empty when at least one
// target counts; otherwise it is insufficient_evidence with Reason
// no_mechanism (Detail says why) or mechanism_unavailable: <cause>.
type Mechanism struct {
	Counted, Unexplained []int64
	Targets              []TargetMechanism
	PlanBasis            string
	Outcome, Reason      string
	Detail               string
}

// Passed reports a gate that lets the attribution judge Counted.
func (m Mechanism) Passed() bool { return m.Outcome == "" }

// MechanismUnavailable is the gate when its evidence could not be read:
// insufficient evidence naming the cause, never a revert on its own.
func MechanismUnavailable(cause error) Mechanism {
	msg := "unknown cause"
	if cause != nil {
		msg = cause.Error()
	}
	return Mechanism{Outcome: OutcomeInsufficient,
		Reason: ReasonMechanismUnavailable + ": " + msg}
}

// MechanismCheck applies the gate. It refuses an unknown class and an
// index step without an index, a step time or a positive cycle; every gap
// in the evidence is a reason.
func MechanismCheck(in MechanismInput) (Mechanism, error) {
	if err := validateMechanism(in); err != nil {
		return Mechanism{}, err
	}
	if !indexClass(in.Class) {
		if in.Metric == "" {
			return noMechanism(nil, "no outcome metric is mapped to this setting"), nil
		}
		return Mechanism{Counted: sortedIDs(in.Targets)}, nil
	}
	index := bareIndexName(in.Index)
	var m Mechanism
	for _, id := range sortedIDs(in.Targets) {
		tm := targetMechanism(in, id, index)
		m.Targets = append(m.Targets, tm)
		if tm.Counted {
			m.Counted = append(m.Counted, id)
			if m.PlanBasis != PlanBasisEstimated {
				m.PlanBasis = tm.PlanBasis
			}
		} else {
			m.Unexplained = append(m.Unexplained, id)
		}
	}
	if len(m.Counted) > 0 {
		return m, nil
	}
	if len(in.Targets) == 0 {
		return noMechanism(nil, "no targets to check"), nil
	}
	return noMechanism(m.Targets, fmt.Sprintf("no target's plan changed within one "+
		"collector cycle of the step to %s %s", gateVerb(in.Class), index)), nil
}

func noMechanism(targets []TargetMechanism, detail string) Mechanism {
	m := Mechanism{Targets: targets, Outcome: OutcomeInsufficient,
		Reason: ReasonNoMechanism, Detail: detail}
	for _, tm := range targets {
		m.Unexplained = append(m.Unexplained, tm.QueryID)
	}
	return m
}

func gateVerb(class string) string {
	if class == MechanismIndexDrop {
		return "stop using"
	}
	return "use"
}

func indexClass(class string) bool {
	return class == MechanismIndexCreate || class == MechanismIndexReplace ||
		class == MechanismIndexDrop
}

func validateMechanism(in MechanismInput) error {
	switch in.Class {
	case MechanismIndexCreate, MechanismIndexReplace, MechanismIndexDrop:
	case MechanismConfigGUC, MechanismAutovacuum, MechanismWALBound:
		return nil
	default:
		return fmt.Errorf("verify: no mechanism rule for action class %q", in.Class)
	}
	switch {
	case bareIndexName(in.Index) == "":
		return fmt.Errorf("verify: %s mechanism needs the index name", in.Class)
	case in.ExecutedAt.IsZero():
		return fmt.Errorf("verify: %s mechanism needs the step time", in.Class)
	case in.Cycle <= 0:
		return fmt.Errorf("verify: %s mechanism needs a positive collector cycle, got %s",
			in.Class, in.Cycle)
	}
	return nil
}

// targetMechanism checks one target: the fingerprint must change within
// one cycle of the step, then the latest plan must name the index (or, for
// a drop, must not).
func targetMechanism(in MechanismInput, id int64, index string) TargetMechanism {
	tm := TargetMechanism{QueryID: id}
	if tm.Reason = planChange(in.PlanHashes[id], in.ExecutedAt, in.Cycle); tm.Reason != "" {
		return tm
	}
	plan, ok := in.LatestPlans[id]
	if !ok || len(plan) == 0 {
		tm.Reason = ReasonMechNoPlan
		return tm
	}
	uses, err := planhash.FindIndex(plan, index)
	if err != nil {
		tm.Reason = ReasonMechNotAPlan + " (" + err.Error() + ")"
		return tm
	}
	drop := in.Class == MechanismIndexDrop
	switch {
	case drop && uses:
		tm.Reason = ReasonMechIndexStillUsed
	case !drop && !uses:
		tm.Reason = ReasonMechIndexUnused
	case drop:
		tm.Counted, tm.Reason = true, ReasonMechDropCounted
	default:
		tm.Counted, tm.Reason = true, ReasonMechCounted
	}
	if tm.Counted {
		tm.PlanBasis = planBasis(plan)
	}
	return tm
}

// planChange is "" when the fingerprint at or before the step differs from
// the first different one after it by at most one cycle; else the reason.
func planChange(samples []PlanSample, step time.Time, cycle time.Duration) string {
	kept := slices.DeleteFunc(slices.Clone(samples), func(s PlanSample) bool {
		return s.Hash == ""
	})
	if len(kept) == 0 {
		return ReasonMechNoHashes
	}
	slices.SortStableFunc(kept, func(a, b PlanSample) int { return a.At.Compare(b.At) })
	before := -1
	for i, s := range kept {
		if !s.At.After(step) {
			before = i
		}
	}
	if before < 0 {
		return ReasonMechNoPlanBefore
	}
	for _, s := range kept[before+1:] {
		if s.Hash != kept[before].Hash {
			if s.At.Sub(step) <= cycle {
				return ""
			}
			return ReasonMechChangedLate
		}
	}
	return ReasonMechUnchanged
}

// planBasis says whether the plan was executed (EXPLAIN ANALYZE or
// auto_explain: actual counters) or only estimated (generic EXPLAIN, the
// only evidence on NEON and Lakebase).
func planBasis(plan []byte) string {
	var doc any
	if json.Unmarshal(plan, &doc) == nil {
		for _, root := range planhash.PlanRoots(doc) {
			if _, ok := root["Actual Rows"]; ok {
				return PlanBasisExecuted
			}
		}
	}
	return PlanBasisEstimated
}

// bareIndexName is the index name EXPLAIN prints: the last dotted part,
// unquoted ("" escapes a quote), and folded to lower case when unquoted.
func bareIndexName(name string) string {
	name = strings.TrimSpace(name)
	last, quoted := 0, false
	for i := 0; i < len(name); i++ {
		switch {
		case name[i] == '"':
			quoted = !quoted
		case name[i] == '.' && !quoted:
			last = i + 1
		}
	}
	part := name[last:]
	if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
		return strings.ReplaceAll(part[1:len(part)-1], `""`, `"`)
	}
	return strings.ToLower(part)
}

// Gate is the verdict a stopped gate gives: insufficient evidence with the
// gate's reason and evidence, never a revert or a retain (lab-01c decides
// the watch's status). stop is false when the gate passed.
func (m Mechanism) Gate() (Verdict, bool) {
	if m.Passed() {
		return Verdict{}, false
	}
	return Verdict{Outcome: m.Outcome, Reason: m.Reason,
		Evidence: map[string]any{"mechanism": m.Evidence()}}, true
}

// Evidence is the gate as JSON-safe data.
func (m Mechanism) Evidence() map[string]any {
	targets := make([]map[string]any, 0, len(m.Targets))
	for _, tm := range m.Targets {
		targets = append(targets, map[string]any{"queryid": tm.QueryID,
			"counted": tm.Counted, "reason": tm.Reason})
	}
	return map[string]any{"counted": nonNilIDs(m.Counted),
		"unexplained": nonNilIDs(m.Unexplained), "targets": targets,
		"plan_basis": m.PlanBasis, "outcome": m.Outcome, "reason": m.Reason,
		"detail": m.Detail}
}

func nonNilIDs(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}
