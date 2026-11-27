package executor

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pg-sage/sidecar/internal/ledger"
	"github.com/pg-sage/sidecar/internal/policy"
)

// recordFailureLogEvery is how often one failing decision is logged. The
// gate evaluates a withheld candidate every cycle, so a decision that
// cannot be recorded fails every cycle; the counter sees each failure.
const recordFailureLogEvery = time.Hour

// recordFailureLog rate-limits the record-failure log by decision
// fingerprint. The zero value is ready to use.
type recordFailureLog struct {
	mu     sync.Mutex
	logged map[string]time.Time
}

// allow reports whether key may be logged at now: never logged, or last
// logged at least recordFailureLogEvery ago. Allowing a line also prunes
// keys whose hour has passed, so the map holds at most an hour of keys.
func (l *recordFailureLog) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, ok := l.logged[key]; ok && now.Sub(last) < recordFailureLogEvery {
		return false
	}
	if l.logged == nil {
		l.logged = map[string]time.Time{}
	}
	for other, last := range l.logged {
		if now.Sub(last) >= recordFailureLogEvery {
			delete(l.logged, other)
		}
	}
	l.logged[key] = now
	return true
}

func (l *recordFailureLog) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.logged)
}

// RecordFailureCount is how many standing-gate decisions of one reason
// could not be recorded in sage.decision for one database.
type RecordFailureCount struct {
	Database string
	Reason   string
	Count    int64
}

var recordFailureCounters = struct {
	mu     sync.Mutex
	counts map[countKey]int64
}{counts: map[countKey]int64{}}

// countRecordFailure counts one decision that could not be recorded. An
// executor without a database name (an embedder's) is not exported.
func countRecordFailure(database, reason string) {
	if database == "" {
		return
	}
	recordFailureCounters.mu.Lock()
	defer recordFailureCounters.mu.Unlock()
	recordFailureCounters.counts[countKey{database, reason}]++
}

// RecordFailureCounts is a sorted snapshot of the record-failure counters.
func RecordFailureCounts() []RecordFailureCount {
	recordFailureCounters.mu.Lock()
	out := make([]RecordFailureCount, 0, len(recordFailureCounters.counts))
	for k, n := range recordFailureCounters.counts {
		out = append(out, RecordFailureCount{Database: k.database, Reason: k.label, Count: n})
	}
	recordFailureCounters.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Database != out[j].Database {
			return out[i].Database < out[j].Database
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// reportRecordFailure is the standing gate's GateConfig.RecordFailed.
func (e *Executor) reportRecordFailure(
	_ context.Context, request policy.ActionRequest, decision policy.Decision, err error,
) {
	e.reportRecordFailureAt(request, decision, err, time.Now())
}

// reportRecordFailureAt counts the failure and logs it at most once per
// decision fingerprint per recordFailureLogEvery.
func (e *Executor) reportRecordFailureAt(
	request policy.ActionRequest, decision policy.Decision, err error, now time.Time,
) {
	countRecordFailure(e.databaseName, string(decision.Reason))
	if !e.recordFailures.allow(e.recordFailureKey(request, decision), now) {
		return
	}
	targets := strings.Join(request.TargetObjs, ", ")
	if targets == "" {
		targets = "no target"
	}
	e.logFn("executor", "could not record the %s decision on %s (database %q): %v; "+
		"the gate answered policy_unavailable (fail closed) and the candidate is "+
		"evaluated again next cycle; this decision is logged again after %s at the earliest",
		decision.LedgerLabel(), targets, e.databaseName, err, recordFailureLogEvery)
}

// recordFailureKey keys a decision as its ledger fingerprint does
// (database, feature, intent, targets, verdict, SQL, principal), leaving
// out the policy version: one failing decision is one key across versions.
func (e *Executor) recordFailureKey(
	request policy.ActionRequest, decision policy.Decision,
) string {
	e.policyMu.RLock()
	databaseID := e.databaseID
	e.policyMu.RUnlock()
	input := ledger.DecisionInput{DatabaseID: databaseID, Feature: request.Feature,
		Intent: ledgerIntent(request), TargetObjects: request.TargetObjs,
		Verdict: ledgerVerdict(decision.Verdict), ProposedSQL: request.SQL}
	if request.Principal != nil {
		input.PrincipalID = request.Principal.ID
	}
	return ledger.DecisionFingerprint(input)
}
