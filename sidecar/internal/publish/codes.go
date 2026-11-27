package publish

import (
	"slices"
	"sort"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/outcomes"
)

// knownActionTypes are the executor action types this build names: every
// earned action class's types and the ledger's labelled ones. Anything
// else in sage.action_log.action_type is a value this build cannot vouch
// for, so it is published as OtherKey.
var knownActionTypes = func() map[string]bool {
	known := map[string]bool{}
	for _, spec := range earned.Classes() {
		for _, t := range spec.ActionTypes {
			known[t] = true
		}
	}
	for _, t := range outcomes.LabelledActionTypes() {
		known[t] = true
	}
	return known
}()

var knownOrigins = []string{outcomes.OriginPlan, outcomes.OriginApproved,
	outcomes.OriginAgent, outcomes.OriginAutonomous}

func classCode(actionType string) string {
	if knownActionTypes[actionType] {
		return actionType
	}
	return OtherKey
}

func originCode(origin string) string {
	if slices.Contains(knownOrigins, origin) {
		return origin
	}
	return OtherKey
}

func verdictCode(verdict string) string {
	if slices.Contains(outcomes.Verdicts(), verdict) {
		return verdict
	}
	return UnknownVerdict
}

// fold turns counts by key into buckets, most changes first (then by
// key), with every key under KFloor added into one OtherKey bucket last.
func fold(byKey map[string]Counts) []Bucket {
	out := []Bucket{}
	var other Counts
	for key, c := range byKey {
		if key == OtherKey || c.Changes < KFloor {
			other = sum(other, c)
			continue
		}
		out = append(out, Bucket{Key: key, Counts: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Changes != out[j].Changes {
			return out[i].Changes > out[j].Changes
		}
		return out[i].Key < out[j].Key
	})
	if other.Changes > 0 {
		out = append(out, Bucket{Key: OtherKey, Counts: other})
	}
	return out
}

func sum(a, b Counts) Counts {
	return Counts{Changes: a.Changes + b.Changes, Improved: a.Improved + b.Improved,
		NoChange: a.NoChange + b.NoChange, Regressed: a.Regressed + b.Regressed,
		Inconclusive: a.Inconclusive + b.Inconclusive, Pending: a.Pending + b.Pending,
		Reverted: a.Reverted + b.Reverted, RollbackFailed: a.RollbackFailed + b.RollbackFailed}
}
