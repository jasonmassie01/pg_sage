package executor

import (
	"sort"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// Evidence keys the ledger stamps on a decision the one-change-per-object
// wait touched. They are reserved: a request's own evidence never sets
// them.
const (
	verificationWaitKey       = "verification_wait"
	verificationWaitDetailKey = "verification_wait_detail"
	verificationOverrideKey   = "verification_override"
	verificationReleasedKey   = "verification_wait_released"
)

// stampVerificationEvidence records what the gate saw: the changes in
// flight that parked the request (or queued it with the wait noted), the
// ones an operator approval overrode, and the ones released at their hard
// deadline.
func stampVerificationEvidence(evidence map[string]any, decision policy.Decision) {
	for _, key := range []string{verificationWaitKey, verificationWaitDetailKey,
		verificationOverrideKey, verificationReleasedKey} {
		delete(evidence, key)
	}
	wait := decision.VerificationWait
	if wait == nil {
		return
	}
	if len(wait.Pending) > 0 {
		if wait.Overridden {
			evidence[verificationOverrideKey] = waitEntries(wait.Pending)
		} else {
			evidence[verificationWaitKey] = waitEntries(wait.Pending)
			evidence[verificationWaitDetailKey] = policy.WaitDetail(wait.Pending)
		}
	}
	if len(wait.Released) > 0 {
		evidence[verificationReleasedKey] = waitEntries(wait.Released)
	}
}

func waitEntries(pending []policy.PendingVerification) []any {
	out := make([]any, 0, len(pending))
	for _, p := range pending {
		out = append(out, map[string]any{"action_id": p.ActionID,
			"decision_id": p.DecisionID, "object": p.Object,
			"until":         p.Until.UTC().Format(time.RFC3339),
			"hard_deadline": p.HardDeadline.UTC().Format(time.RFC3339)})
	}
	return out
}

// ParkCount is how many times the gate parked a change on one database for
// one reason (each evaluation of a parked candidate counts).
type ParkCount struct {
	Database string
	Reason   string
	Count    int64
}

// WaitReleaseCount is how many verification waits ended without a verdict,
// by cause: operator_override or hard_deadline.
type WaitReleaseCount struct {
	Database string
	Cause    string
	Count    int64
}

type countKey struct{ database, label string }

var parkCounters = struct {
	mu       sync.Mutex
	parks    map[countKey]int64
	releases map[countKey]int64
}{parks: map[countKey]int64{}, releases: map[countKey]int64{}}

// countDecision counts a recorded standing-gate decision for /metrics. An
// executor without a database name (an embedder's) is not exported: every
// series carries the database it belongs to.
func countDecision(database string, decision policy.Decision) {
	if database == "" {
		return
	}
	parkCounters.mu.Lock()
	defer parkCounters.mu.Unlock()
	if decision.Verdict == policy.VerdictPark {
		parkCounters.parks[countKey{database, string(decision.Reason)}]++
	}
	wait := decision.VerificationWait
	if wait == nil {
		return
	}
	if wait.Overridden && len(wait.Pending) > 0 {
		parkCounters.releases[countKey{database, "operator_override"}]++
	}
	if len(wait.Released) > 0 {
		parkCounters.releases[countKey{database, "hard_deadline"}] += int64(len(wait.Released))
	}
}

// ParkCounts is a sorted snapshot of the park counters.
func ParkCounts() []ParkCount {
	parkCounters.mu.Lock()
	out := make([]ParkCount, 0, len(parkCounters.parks))
	for k, n := range parkCounters.parks {
		out = append(out, ParkCount{Database: k.database, Reason: k.label, Count: n})
	}
	parkCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// WaitReleaseCounts is a sorted snapshot of the wait release counters.
func WaitReleaseCounts() []WaitReleaseCount {
	parkCounters.mu.Lock()
	out := make([]WaitReleaseCount, 0, len(parkCounters.releases))
	for k, n := range parkCounters.releases {
		out = append(out, WaitReleaseCount{Database: k.database, Cause: k.label, Count: n})
	}
	parkCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Cause < out[j].Cause
	})
	return out
}
