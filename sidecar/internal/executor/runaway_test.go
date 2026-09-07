package executor

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// nopLog is a no-op logger satisfying the logFn signature.
func nopLog(string, string, ...any) {}

// defaultPolicy returns a simple RunawayPolicy for tests. WarnCycles=2,
// CancelCycles=2 means: after warning, 2 more cycles to cancel; after
// cancel, 2 more cycles to terminate.
func defaultPolicy() config.RunawayPolicy {
	return config.RunawayPolicy{
		Name:               "default",
		MaxDurationMinutes: 5,
		MaxBlockedSessions: 0,
		WarnCycles:         2,
		CancelCycles:       2,
	}
}

// defaultCfg returns a RunawayConfig with one default policy.
func defaultCfg() *config.RunawayConfig {
	return &config.RunawayConfig{
		Enabled:      true,
		Policies:     []config.RunawayPolicy{defaultPolicy()},
		SafePatterns: []string{"pg_dump", "replication"},
	}
}

// makeActiveQuery creates an ActiveQuery exceeding the default policy's
// MaxDurationMinutes threshold.
func makeActiveQuery(pid int, start time.Time, appName string) ActiveQuery {
	return ActiveQuery{
		PID:        pid,
		QueryStart: start,
		QueryID:    12345,
		Query:      "SELECT pg_sleep(600)",
		AppName:    appName,
		Duration:   6 * time.Minute, // exceeds 5-minute policy
		State:      "active",
	}
}

// --- isSafeRunawayProcess tests ---

func TestIsSafeRunawayProcess_OwnPID(t *testing.T) {
	// Own PID should always be safe regardless of app name or patterns.
	got := isSafeRunawayProcess("random_app", 42, 42, nil)
	if !got {
		t.Error("expected own PID to be safe, got false")
	}

	// Even with empty patterns and empty app name.
	got = isSafeRunawayProcess("", 100, 100, []string{})
	if !got {
		t.Error("expected own PID to be safe with empty app name")
	}
}

func TestIsSafeRunawayProcess_PatternMatch(t *testing.T) {
	patterns := []string{"pg_dump", "replication"}

	tests := []struct {
		name    string
		appName string
		want    bool
	}{
		{"exact lowercase", "pg_dump", true},
		{"case insensitive upper", "PG_DUMP", true},
		{"mixed case", "Pg_Dump_Worker", true},
		{"substring match", "my_replication_slot", true},
		{"replication mixed case", "REPLICATION_STREAM", true},
		{"no match", "my_application", false},
		{"empty app name", "", false},
		{"partial no match", "pg_dum", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSafeRunawayProcess(tt.appName, 999, 1, patterns)
			if got != tt.want {
				t.Errorf("isSafeRunawayProcess(%q) = %v, want %v",
					tt.appName, got, tt.want)
			}
		})
	}
}

func TestIsSafeRunawayProcess_NoMatch(t *testing.T) {
	patterns := []string{"pg_dump", "replication"}
	got := isSafeRunawayProcess("my_web_app", 500, 1, patterns)
	if got {
		t.Error("expected non-matching app name to be unsafe, got true")
	}
}

// --- truncateQuery tests ---

func TestTruncateQuery_Short(t *testing.T) {
	input := "SELECT 1"
	got := truncateQuery(input, 200)
	if got != input {
		t.Errorf("truncateQuery(%q, 200) = %q, want %q", input, got, input)
	}
}

func TestTruncateQuery_ExactLength(t *testing.T) {
	input := "12345"
	got := truncateQuery(input, 5)
	if got != input {
		t.Errorf("truncateQuery(%q, 5) = %q, want %q", input, got, input)
	}
}

func TestTruncateQuery_Long(t *testing.T) {
	input := "SELECT very_long_column FROM very_long_table WHERE x = 1"
	got := truncateQuery(input, 20)

	if len(got) != 20 {
		t.Errorf("expected length 20, got %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected '...' suffix, got %q", got)
	}
	// First 17 chars preserved, then "..."
	want := input[:17] + "..."
	if got != want {
		t.Errorf("truncateQuery = %q, want %q", got, want)
	}
}

func TestTruncateQuery_VeryShortLimit(t *testing.T) {
	input := "SELECT 1"

	tests := []struct {
		maxLen int
		want   string
	}{
		{3, "SEL"}, // maxLen == 3: no room for "...", just truncate
		{2, "SE"},  // maxLen < 3
		{1, "S"},   // maxLen == 1
		{0, ""},    // maxLen == 0
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("maxLen=%d", tt.maxLen), func(t *testing.T) {
			got := truncateQuery(input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateQuery(%q, %d) = %q, want %q",
					input, tt.maxLen, got, tt.want)
			}
		})
	}
}

// --- Evaluate state machine tests ---

func TestEvaluate_NoActiveQueries(t *testing.T) {
	rt := NewRunawayTracker(defaultCfg(), 1, nopLog)
	findings := rt.Evaluate(nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestEvaluate_BelowPolicyThreshold(t *testing.T) {
	rt := NewRunawayTracker(defaultCfg(), 1, nopLog)
	start := time.Now().Add(-2 * time.Minute)
	aq := ActiveQuery{
		PID:        100,
		QueryStart: start,
		QueryID:    1,
		Query:      "SELECT 1",
		AppName:    "app",
		Duration:   2 * time.Minute, // below 5-minute threshold
		State:      "active",
	}

	// Run multiple cycles; should never produce findings.
	for i := 0; i < 10; i++ {
		findings := rt.Evaluate([]ActiveQuery{aq}, nil)
		if len(findings) != 0 {
			t.Errorf("cycle %d: expected 0 findings for below-threshold query, got %d",
				i, len(findings))
		}
	}
}

func TestEvaluate_SafeProcessSkipped(t *testing.T) {
	cfg := defaultCfg()
	ownPID := 42
	rt := NewRunawayTracker(cfg, ownPID, nopLog)
	start := time.Now().Add(-10 * time.Minute)

	queries := []ActiveQuery{
		// Own PID -- should be skipped.
		makeActiveQuery(ownPID, start, "pg_sage"),
		// Safe pattern -- should be skipped.
		makeActiveQuery(200, start, "pg_dump_worker"),
		// Safe pattern case insensitive.
		makeActiveQuery(201, start, "MY_REPLICATION_SLOT"),
	}

	// Run enough cycles for escalation if they weren't safe.
	for i := 0; i < 10; i++ {
		findings := rt.Evaluate(queries, nil)
		if len(findings) != 0 {
			t.Errorf("cycle %d: expected 0 findings for safe processes, got %d",
				i, len(findings))
		}
	}
}

func TestEvaluate_FirstSeen_NoFinding(t *testing.T) {
	rt := NewRunawayTracker(defaultCfg(), 1, nopLog)
	start := time.Now().Add(-10 * time.Minute)
	aq := makeActiveQuery(100, start, "app")

	// First cycle: query gets tracked but no finding yet.
	findings := rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 0 {
		t.Errorf("first cycle should produce 0 findings, got %d", len(findings))
	}
}

func TestEvaluate_WarnAfterObservation(t *testing.T) {
	rt := NewRunawayTracker(defaultCfg(), 1, nopLog)
	start := time.Now().Add(-10 * time.Minute)
	aq := makeActiveQuery(100, start, "my_app")

	// Cycle 1: first seen, tracked, no finding.
	findings := rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 0 {
		t.Fatalf("cycle 1: expected 0 findings, got %d", len(findings))
	}

	// Cycle 2: cycle - FirstSeenCycle == 1, still below threshold of 2.
	findings = rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 0 {
		t.Fatalf("cycle 2: expected 0 findings, got %d", len(findings))
	}

	// Cycle 3: cycle - FirstSeenCycle == 2 >= 2, transitions to "warned".
	findings = rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 1 {
		t.Fatalf("cycle 3: expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.Severity != "warning" {
		t.Errorf("expected severity 'warning', got %q", f.Severity)
	}
	if f.Category != "runaway_query" {
		t.Errorf("expected category 'runaway_query', got %q", f.Category)
	}
	if f.RecommendedSQL != "" {
		t.Errorf("warned state should have no SQL, got %q", f.RecommendedSQL)
	}

	detail, ok := f.Detail["state"]
	if !ok || detail != "warned" {
		t.Errorf("expected detail state 'warned', got %v", detail)
	}
}

func TestEvaluate_CancelAfterWarnCycles(t *testing.T) {
	cfg := defaultCfg()
	cfg.Policies[0].WarnCycles = 2
	rt := NewRunawayTracker(cfg, 1, nopLog)
	start := time.Now().Add(-10 * time.Minute)
	aq := makeActiveQuery(100, start, "app")

	// Advance to warned state (3 cycles: first seen + 2 observation).
	for i := 0; i < 3; i++ {
		rt.Evaluate([]ActiveQuery{aq}, nil)
	}

	// Verify we're in warned state.
	findings := rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding in warned state, got %d", len(findings))
	}
	if findings[0].Detail["state"] != "warned" {
		t.Fatalf("expected state 'warned', got %v", findings[0].Detail["state"])
	}

	// WarnCycles=2: need 2 cycles from warned to cancel.
	// WarnedAtCycle was set at cycle 3. We're now at cycle 4.
	// cycle 4: cycle-WarnedAtCycle = 1 < 2, still warned.
	// cycle 5: cycle-WarnedAtCycle = 2 >= 2, transitions to cancelled.
	findings = rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.Detail["state"] != "cancelled" {
		t.Errorf("expected state 'cancelled', got %v", f.Detail["state"])
	}
	if f.Severity != "critical" {
		t.Errorf("expected severity 'critical', got %q", f.Severity)
	}
	wantSQL := fmt.Sprintf("SELECT pg_cancel_backend(%d);", 100)
	if f.RecommendedSQL != wantSQL {
		t.Errorf("expected SQL %q, got %q", wantSQL, f.RecommendedSQL)
	}
	if f.ActionRisk != "safe" {
		t.Errorf("expected risk 'safe', got %q", f.ActionRisk)
	}
}

func TestEvaluate_TerminateAfterCancelCycles(t *testing.T) {
	cfg := defaultCfg()
	cfg.Policies[0].WarnCycles = 1
	cfg.Policies[0].CancelCycles = 1
	rt := NewRunawayTracker(cfg, 1, nopLog)
	start := time.Now().Add(-10 * time.Minute)
	aq := makeActiveQuery(100, start, "app")

	// Cycle 1: first seen.
	// Cycle 2: observation (cycle-first=1 < 2, still "").
	// Cycle 3: cycle-first=2 >= 2, -> warned. WarnedAtCycle=3.
	// Cycle 4: cycle-warned=1 >= 1 (WarnCycles=1), -> cancelled.
	//          CancelledAtCycle=4.
	// Cycle 5: cycle-cancelled=1 >= 1 (CancelCycles=1), -> terminated.

	var lastFindings []string
	for i := 0; i < 5; i++ {
		findings := rt.Evaluate([]ActiveQuery{aq}, nil)
		lastFindings = lastFindings[:0]
		for _, f := range findings {
			lastFindings = append(lastFindings,
				fmt.Sprintf("%v", f.Detail["state"]))
		}
	}

	// After 5 cycles we should be at terminated.
	findings := rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.Detail["state"] != "terminated" {
		t.Errorf("expected state 'terminated', got %v", f.Detail["state"])
	}
	if f.Severity != "critical" {
		t.Errorf("expected severity 'critical', got %q", f.Severity)
	}
	wantSQL := fmt.Sprintf("SELECT pg_terminate_backend(%d);", 100)
	if f.RecommendedSQL != wantSQL {
		t.Errorf("expected SQL %q, got %q", wantSQL, f.RecommendedSQL)
	}
	if f.ActionRisk != "moderate" {
		t.Errorf("expected risk 'moderate', got %q", f.ActionRisk)
	}
}

func TestEvaluate_QueryFinishes_Pruned(t *testing.T) {
	rt := NewRunawayTracker(defaultCfg(), 1, nopLog)
	start := time.Now().Add(-10 * time.Minute)
	aq := makeActiveQuery(100, start, "app")

	// Track for a few cycles.
	for i := 0; i < 3; i++ {
		rt.Evaluate([]ActiveQuery{aq}, nil)
	}

	// Now the query finishes: empty active list.
	findings := rt.Evaluate(nil, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings after query finished, got %d",
			len(findings))
	}

	// Verify internal state is pruned by checking that the query
	// would restart fresh if it appeared again.
	findings = rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings on re-appearance (first seen), got %d",
			len(findings))
	}
}

func TestEvaluate_PIDReuse(t *testing.T) {
	rt := NewRunawayTracker(defaultCfg(), 1, nopLog)

	start1 := time.Now().Add(-10 * time.Minute)
	start2 := time.Now().Add(-1 * time.Minute) // different QueryStart

	aq1 := makeActiveQuery(100, start1, "app1")
	aq2 := makeActiveQuery(100, start2, "app2")
	aq2.Duration = 6 * time.Minute // also exceeds threshold

	// Track aq1 for a few cycles until warned.
	for i := 0; i < 3; i++ {
		rt.Evaluate([]ActiveQuery{aq1}, nil)
	}

	// Now aq1 disappears and aq2 appears with same PID but different start.
	findings := rt.Evaluate([]ActiveQuery{aq2}, nil)

	// aq1 should be pruned. aq2 is first seen, no finding.
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for new query on reused PID, got %d",
			len(findings))
	}

	// aq2 should escalate independently.
	rt.Evaluate([]ActiveQuery{aq2}, nil)            // cycle +1
	findings = rt.Evaluate([]ActiveQuery{aq2}, nil) // cycle +2 from first seen
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding after aq2 escalation, got %d",
			len(findings))
	}
	if findings[0].Detail["state"] != "warned" {
		t.Errorf("expected 'warned' for aq2, got %v",
			findings[0].Detail["state"])
	}
}

func TestEvaluate_BlockedSessionsPolicy(t *testing.T) {
	cfg := &config.RunawayConfig{
		Enabled: true,
		Policies: []config.RunawayPolicy{
			{
				Name:               "blocker",
				MaxDurationMinutes: 0, // duration not checked
				MaxBlockedSessions: 3,
				WarnCycles:         1,
				CancelCycles:       1,
			},
		},
	}
	rt := NewRunawayTracker(cfg, 1, nopLog)
	start := time.Now().Add(-1 * time.Minute)

	aq := ActiveQuery{
		PID:        200,
		QueryStart: start,
		QueryID:    99,
		Query:      "UPDATE big_table SET x = 1",
		AppName:    "blocker_app",
		Duration:   1 * time.Minute, // short duration, but blocking
		State:      "active",
	}

	blockers := map[int]int{200: 5} // PID 200 blocks 5 sessions (>= 3)

	// Cycle 1: first seen.
	findings := rt.Evaluate([]ActiveQuery{aq}, blockers)
	if len(findings) != 0 {
		t.Fatalf("cycle 1: expected 0 findings, got %d", len(findings))
	}

	// Cycle 2: still observation.
	findings = rt.Evaluate([]ActiveQuery{aq}, blockers)
	if len(findings) != 0 {
		t.Fatalf("cycle 2: expected 0 findings, got %d", len(findings))
	}

	// Cycle 3: cycle-first=2 >= 2, -> warned.
	findings = rt.Evaluate([]ActiveQuery{aq}, blockers)
	if len(findings) != 1 {
		t.Fatalf("cycle 3: expected 1 finding, got %d", len(findings))
	}
	if findings[0].Detail["state"] != "warned" {
		t.Errorf("expected 'warned', got %v", findings[0].Detail["state"])
	}

	// Cycle 4: WarnCycles=1, cycle-warned=1 >= 1, -> cancelled.
	findings = rt.Evaluate([]ActiveQuery{aq}, blockers)
	if len(findings) != 1 {
		t.Fatalf("cycle 4: expected 1 finding, got %d", len(findings))
	}
	if findings[0].Detail["state"] != "cancelled" {
		t.Errorf("expected 'cancelled', got %v", findings[0].Detail["state"])
	}
}

func TestEvaluate_MultiplePolicies_BestMatch(t *testing.T) {
	cfg := &config.RunawayConfig{
		Enabled: true,
		Policies: []config.RunawayPolicy{
			{
				Name:               "slow",
				MaxDurationMinutes: 5,
				WarnCycles:         5,
				CancelCycles:       5, // total = 10
			},
			{
				Name:               "fast",
				MaxDurationMinutes: 5,
				WarnCycles:         1,
				CancelCycles:       1, // total = 2 (best)
			},
		},
	}
	rt := NewRunawayTracker(cfg, 1, nopLog)
	start := time.Now().Add(-10 * time.Minute)
	aq := makeActiveQuery(100, start, "app")

	// Cycle 1: first seen.
	rt.Evaluate([]ActiveQuery{aq}, nil)

	// The tracker records MatchedPolicy on first seen. Check that on
	// first tracking it picks the best (shortest total) policy.
	rt.mu.Lock()
	key := trackerKey{PID: 100, QueryStart: start}
	tq, ok := rt.tracked[key]
	rt.mu.Unlock()

	if !ok {
		t.Fatal("expected query to be tracked")
	}
	if tq.MatchedPolicy != "fast" {
		t.Errorf("expected MatchedPolicy 'fast', got %q", tq.MatchedPolicy)
	}

	// "fast" policy: WarnCycles=1, CancelCycles=1
	// Cycle 2: observation.
	rt.Evaluate([]ActiveQuery{aq}, nil)

	// Cycle 3: warned (cycle-first=2 >= 2).
	findings := rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 1 || findings[0].Detail["state"] != "warned" {
		t.Fatalf("expected warned, got %v", findings)
	}

	// Cycle 4: WarnCycles=1 for "fast", cancel.
	findings = rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 1 || findings[0].Detail["state"] != "cancelled" {
		t.Fatalf("expected cancelled, got %v", findings)
	}

	// Cycle 5: CancelCycles=1, terminate.
	findings = rt.Evaluate([]ActiveQuery{aq}, nil)
	if len(findings) != 1 || findings[0].Detail["state"] != "terminated" {
		t.Fatalf("expected terminated, got %v", findings)
	}
}

func TestEvaluate_ConcurrentSafe(t *testing.T) {
	rt := NewRunawayTracker(defaultCfg(), 1, nopLog)
	start := time.Now().Add(-10 * time.Minute)

	var wg sync.WaitGroup
	goroutines := 20

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			aq := makeActiveQuery(100+id, start, "app")
			for i := 0; i < 10; i++ {
				_ = rt.Evaluate([]ActiveQuery{aq}, nil)
			}
		}(g)
	}

	wg.Wait()
	// If we get here without a data race panic (run with -race), pass.
}

// --- buildRunawayFinding tests ---

func TestBuildRunawayFinding_Warned(t *testing.T) {
	tq := &TrackedQuery{
		PID:            100,
		QueryStart:     time.Now().Add(-6 * time.Minute),
		QueryID:        1,
		QueryText:      "SELECT pg_sleep(600)",
		AppName:        "test_app",
		FirstSeenCycle: 1,
		MatchedPolicy:  "default",
		State:          "warned",
		WarnedAtCycle:  3,
	}

	f := buildRunawayFinding(tq, 4)

	if f.Category != "runaway_query" {
		t.Errorf("expected category 'runaway_query', got %q", f.Category)
	}
	if f.Severity != "warning" {
		t.Errorf("expected severity 'warning', got %q", f.Severity)
	}
	if f.RecommendedSQL != "" {
		t.Errorf("warned finding should have no SQL, got %q", f.RecommendedSQL)
	}
	if f.ActionRisk != "" {
		t.Errorf("warned finding should have empty risk, got %q", f.ActionRisk)
	}
	if !strings.Contains(f.Recommendation, "Monitoring") {
		t.Errorf("expected narrative to mention 'Monitoring', got %q",
			f.Recommendation)
	}
	if f.ObjectType != "process" {
		t.Errorf("expected ObjectType 'process', got %q", f.ObjectType)
	}
	if !strings.Contains(f.ObjectIdentifier, "pid:100") {
		t.Errorf("expected ObjectIdentifier to contain 'pid:100', got %q",
			f.ObjectIdentifier)
	}
	if !strings.Contains(f.ObjectIdentifier, "policy:default") {
		t.Errorf("expected ObjectIdentifier to contain 'policy:default', got %q",
			f.ObjectIdentifier)
	}

	// Detail assertions.
	if f.Detail["pid"] != 100 {
		t.Errorf("expected detail pid=100, got %v", f.Detail["pid"])
	}
	if f.Detail["state"] != "warned" {
		t.Errorf("expected detail state='warned', got %v", f.Detail["state"])
	}
	if f.Detail["policy"] != "default" {
		t.Errorf("expected detail policy='default', got %v", f.Detail["policy"])
	}
	cycleCount, ok := f.Detail["cycle_count"].(uint64)
	if !ok || cycleCount != 3 {
		t.Errorf("expected cycle_count=3, got %v", f.Detail["cycle_count"])
	}
}

func TestBuildRunawayFinding_Cancelled(t *testing.T) {
	tq := &TrackedQuery{
		PID:              200,
		QueryStart:       time.Now().Add(-10 * time.Minute),
		QueryID:          2,
		QueryText:        "UPDATE foo SET bar = 1",
		AppName:          "batch_app",
		FirstSeenCycle:   1,
		MatchedPolicy:    "heavy",
		State:            "cancelled",
		WarnedAtCycle:    3,
		CancelledAtCycle: 5,
	}

	f := buildRunawayFinding(tq, 6)

	if f.Severity != "critical" {
		t.Errorf("expected severity 'critical', got %q", f.Severity)
	}

	wantSQL := "SELECT pg_cancel_backend(200);"
	if f.RecommendedSQL != wantSQL {
		t.Errorf("expected SQL %q, got %q", wantSQL, f.RecommendedSQL)
	}
	if f.ActionRisk != "safe" {
		t.Errorf("expected risk 'safe', got %q", f.ActionRisk)
	}
	if !strings.Contains(f.Recommendation, "pg_cancel_backend") {
		t.Errorf("expected narrative to mention pg_cancel_backend, got %q",
			f.Recommendation)
	}
	if f.Detail["state"] != "cancelled" {
		t.Errorf("expected detail state 'cancelled', got %v",
			f.Detail["state"])
	}
}

func TestBuildRunawayFinding_Terminated(t *testing.T) {
	tq := &TrackedQuery{
		PID:              300,
		QueryStart:       time.Now().Add(-15 * time.Minute),
		QueryID:          3,
		QueryText:        "DELETE FROM huge_table",
		AppName:          "cleanup_job",
		FirstSeenCycle:   1,
		MatchedPolicy:    "critical_policy",
		State:            "terminated",
		WarnedAtCycle:    3,
		CancelledAtCycle: 5,
	}

	f := buildRunawayFinding(tq, 8)

	if f.Severity != "critical" {
		t.Errorf("expected severity 'critical', got %q", f.Severity)
	}

	wantSQL := "SELECT pg_terminate_backend(300);"
	if f.RecommendedSQL != wantSQL {
		t.Errorf("expected SQL %q, got %q", wantSQL, f.RecommendedSQL)
	}
	if f.ActionRisk != "moderate" {
		t.Errorf("expected risk 'moderate', got %q", f.ActionRisk)
	}
	if !strings.Contains(f.Recommendation, "pg_terminate_backend") {
		t.Errorf("expected narrative to mention pg_terminate_backend, got %q",
			f.Recommendation)
	}
	if f.Detail["state"] != "terminated" {
		t.Errorf("expected detail state 'terminated', got %v",
			f.Detail["state"])
	}
	if f.RollbackSQL != "" {
		t.Errorf("expected empty RollbackSQL, got %q", f.RollbackSQL)
	}
}
