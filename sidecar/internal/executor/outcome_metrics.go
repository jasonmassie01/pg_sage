package executor

import (
	"sort"
	"sync"

	"github.com/pg-sage/sidecar/internal/policy"
)

// observability-04: every terminal action_log outcome and every finalized
// verification verdict is counted for /metrics, after its write commits,
// so a write that fails or rolls back never counts. Labels are bounded:
// the class is a policy change class (or other), the outcome and verdict
// fold into short fixed sets, and non-terminal states are not counted.
// Counters are process memory: they reset on restart, as Prometheus
// counters do, and in HA only the executing leader counts.

// OutcomeCount is how many actions of one class on one database ended in
// one outcome.
type OutcomeCount struct {
	Database string
	Class    string
	Outcome  string
	Count    int64
}

// VerdictCount is how many verifications of one class on one database
// were finalized with one verdict.
type VerdictCount struct {
	Database string
	Class    string
	Verdict  string
	Count    int64
}

// classOther is the class, outcome and verdict label of anything unknown.
const classOther = "other"

// metricsClasses are the policy's change classes plus other.
var metricsClasses = func() map[string]bool {
	classes := []policy.ChangeClass{policy.ChangeIndex, policy.ChangeAnalyze,
		policy.ChangeVacuum, policy.ChangeFreeze, policy.ChangeAutovacuumTuning,
		policy.ChangeConfigGUC, policy.ChangeRetention, policy.ChangeFKIndex,
		policy.ChangeOnlineMigration, policy.ChangeBackendSignal, policy.ChangeQueryHint,
		policy.ChangeSchemaChange}
	out := map[string]bool{classOther: true}
	for _, c := range append(classes, policy.AgentChangeClasses()...) {
		out[string(c)] = true
	}
	return out
}()

// outcomeLabels are the outcome label values.
var outcomeLabels = map[string]bool{"success": true, "failed": true, "rolled_back": true,
	"cancelled": true, "unverifiable": true, classOther: true}

// verdictLabels are the verification verdict label values.
var verdictLabels = map[string]bool{"success": true, "revert": true, "unverifiable": true,
	"failed": true, classOther: true}

// nonTerminalOutcomes are action_log states of an action that has not
// ended; they are never counted.
var nonTerminalOutcomes = map[string]bool{"": true, "pending": true, "monitoring": true,
	"rolling_back": true, "interrupted": true, "applied_pending_restart": true}

// outcomeFolds maps action_log outcomes onto the label an alert names: a
// partial change or a failed rollback left something to fix (failed); an
// index already back is rolled back; an expired watch is unverifiable.
var outcomeFolds = map[string]string{"partial": "failed", "rollback_failed": "failed",
	"reverted": "rolled_back", "already_restored": "rolled_back",
	"verification_expired": "unverifiable"}

// outcomeLabel is the label of an action_log outcome; terminal is false for
// a state that is not an outcome yet.
func outcomeLabel(outcome string) (label string, terminal bool) {
	if nonTerminalOutcomes[outcome] {
		return "", false
	}
	if folded, ok := outcomeFolds[outcome]; ok {
		return folded, true
	}
	if outcomeLabels[outcome] {
		return outcome, true
	}
	return classOther, true
}

func boundedLabel(value string, known map[string]bool) string {
	if known[value] {
		return value
	}
	return classOther
}

type outcomeKey struct{ database, class, label string }

var outcomeCounters = struct {
	mu       sync.Mutex
	outcomes map[outcomeKey]int64
	verdicts map[outcomeKey]int64
}{outcomes: map[outcomeKey]int64{}, verdicts: map[outcomeKey]int64{}}

// RecordOutcome counts one action of class on database that ended in
// outcome (an action_log outcome). Call it once the write committed. A
// non-terminal state, or a database without a name, records nothing.
func RecordOutcome(database, class, outcome string) {
	label, terminal := outcomeLabel(outcome)
	if database == "" || !terminal {
		return
	}
	key := outcomeKey{database, boundedLabel(class, metricsClasses), label}
	outcomeCounters.mu.Lock()
	outcomeCounters.outcomes[key]++
	outcomeCounters.mu.Unlock()
}

// RecordVerdict counts one finalized verification verdict (success,
// revert, unverifiable, failed; anything else is other).
func RecordVerdict(database, class, verdict string) {
	if database == "" {
		return
	}
	key := outcomeKey{database, boundedLabel(class, metricsClasses),
		boundedLabel(verdict, verdictLabels)}
	outcomeCounters.mu.Lock()
	outcomeCounters.verdicts[key]++
	outcomeCounters.mu.Unlock()
}

// OutcomeCounts is a snapshot of the outcome counters, sorted by
// database, class and outcome.
func OutcomeCounts() []OutcomeCount {
	outcomeCounters.mu.Lock()
	out := make([]OutcomeCount, 0, len(outcomeCounters.outcomes))
	for k, n := range outcomeCounters.outcomes {
		out = append(out, OutcomeCount{Database: k.database, Class: k.class,
			Outcome: k.label, Count: n})
	}
	outcomeCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return lessKey(outcomeKey{out[i].Database, out[i].Class, out[i].Outcome},
			outcomeKey{out[j].Database, out[j].Class, out[j].Outcome})
	})
	return out
}

// VerdictCounts is a snapshot of the verdict counters, sorted like
// OutcomeCounts.
func VerdictCounts() []VerdictCount {
	outcomeCounters.mu.Lock()
	out := make([]VerdictCount, 0, len(outcomeCounters.verdicts))
	for k, n := range outcomeCounters.verdicts {
		out = append(out, VerdictCount{Database: k.database, Class: k.class,
			Verdict: k.label, Count: n})
	}
	outcomeCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return lessKey(outcomeKey{out[i].Database, out[i].Class, out[i].Verdict},
			outcomeKey{out[j].Database, out[j].Class, out[j].Verdict})
	})
	return out
}

func lessKey(a, b outcomeKey) bool {
	if a.database != b.database {
		return a.database < b.database
	}
	if a.class != b.class {
		return a.class < b.class
	}
	return a.label < b.label
}

// pruneOutcomeCounts drops database's outcome and verdict series.
func pruneOutcomeCounts(database string) {
	outcomeCounters.mu.Lock()
	defer outcomeCounters.mu.Unlock()
	for _, counts := range []map[outcomeKey]int64{outcomeCounters.outcomes,
		outcomeCounters.verdicts} {
		for k := range counts {
			if k.database == database {
				delete(counts, k)
			}
		}
	}
}
