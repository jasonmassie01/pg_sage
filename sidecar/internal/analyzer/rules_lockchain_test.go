package analyzer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// isSafeProcess
// ---------------------------------------------------------------------------

func TestIsSafeProcess_OwnPID(t *testing.T) {
	// Own PID always returns true regardless of app name or patterns.
	got := isSafeProcess("random_app", 42, 42, nil)
	if !got {
		t.Error("expected true when pid == ownPID, got false")
	}
}

func TestIsSafeProcess_PatternMatch(t *testing.T) {
	tests := []struct {
		name     string
		appName  string
		patterns []string
		want     bool
	}{
		{
			name:     "exact lowercase match",
			appName:  "patroni",
			patterns: []string{"patroni"},
			want:     true,
		},
		{
			name:     "case insensitive match",
			appName:  "Patroni-Leader",
			patterns: []string{"patroni"},
			want:     true,
		},
		{
			name:     "substring match",
			appName:  "pg_basebackup_slot_replication",
			patterns: []string{"replication"},
			want:     true,
		},
		{
			name:     "pattern uppercase app lowercase",
			appName:  "walreceiver",
			patterns: []string{"WALRECEIVER"},
			want:     true,
		},
		{
			name:     "multiple patterns second matches",
			appName:  "pg_dump",
			patterns: []string{"replication", "pg_dump", "patroni"},
			want:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSafeProcess(tt.appName, 999, 1, tt.patterns)
			if got != tt.want {
				t.Errorf("isSafeProcess(%q, 999, 1, %v) = %v, want %v",
					tt.appName, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestIsSafeProcess_NoMatch(t *testing.T) {
	got := isSafeProcess("my_django_app", 100, 1, []string{"patroni", "replication"})
	if got {
		t.Error("expected false for unmatched app name, got true")
	}
}

func TestIsSafeProcess_EmptyPatterns(t *testing.T) {
	// With no patterns and a non-matching PID, should return false.
	got := isSafeProcess("some_app", 100, 1, []string{})
	if got {
		t.Error("expected false with empty patterns, got true")
	}

	// Also nil patterns.
	got = isSafeProcess("some_app", 100, 1, nil)
	if got {
		t.Error("expected false with nil patterns, got true")
	}
}

// ---------------------------------------------------------------------------
// truncateQuery
// ---------------------------------------------------------------------------

func TestTruncateQuery_Short(t *testing.T) {
	q := "SELECT 1"
	got := truncateQuery(q, 100)
	if got != q {
		t.Errorf("truncateQuery(%q, 100) = %q, want %q", q, got, q)
	}
}

func TestTruncateQuery_ExactLimit(t *testing.T) {
	q := "SELECT * FROM orders WHERE id = 42" // 35 chars
	got := truncateQuery(q, len(q))
	if got != q {
		t.Errorf("truncateQuery at exact limit: got %q, want %q", got, q)
	}
}

func TestTruncateQuery_Long(t *testing.T) {
	q := strings.Repeat("x", 250)
	got := truncateQuery(q, 200)
	if len(got) != 203 { // 200 + len("...")
		t.Errorf("expected length 203, got %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Error("expected truncated query to end with '...'")
	}
	if got[:200] != q[:200] {
		t.Error("truncated prefix does not match original")
	}
}

func TestTruncateQuery_EmptyString(t *testing.T) {
	got := truncateQuery("", 100)
	if got != "" {
		t.Errorf("truncateQuery empty: got %q, want %q", got, "")
	}
}

// ---------------------------------------------------------------------------
// lockChainFindings
// ---------------------------------------------------------------------------

func baseLockChainConfig() config.LockChainConfig {
	return config.LockChainConfig{
		Enabled:                  true,
		MinBlockedThreshold:      3,
		CriticalBlockedThreshold: 10,
		IdleInTxTerminateMinutes: 5,
		ActiveQueryCancelMinutes: 15,
		SafePatterns:             []string{"patroni", "replication"},
	}
}

func TestLockChainFindings_BelowThreshold(t *testing.T) {
	lcCfg := baseLockChainConfig()
	chains := []LockChain{
		{
			RootBlockerPID:   100,
			RootBlockerApp:   "my_app",
			RootBlockerState: "active",
			RootBlockerSince: time.Now().Add(-1 * time.Minute),
			TotalBlocked:     2, // below MinBlockedThreshold of 3
			BlockedPIDs:      []int{200, 201},
			ChainDepth:       1,
		},
	}

	findings := lockChainFindings(chains, lcCfg, 1)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for below-threshold chain, got %d", len(findings))
	}
}

func TestLockChainFindings_SafeProcess(t *testing.T) {
	lcCfg := baseLockChainConfig()
	chains := []LockChain{
		{
			RootBlockerPID:   100,
			RootBlockerApp:   "patroni-leader",
			RootBlockerState: "idle",
			RootBlockerSince: time.Now().Add(-10 * time.Minute),
			TotalBlocked:     5,
			BlockedPIDs:      []int{200, 201, 202, 203, 204},
			ChainDepth:       2,
		},
	}

	findings := lockChainFindings(chains, lcCfg, 1)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.Severity != "info" {
		t.Errorf("safe process severity: got %q, want %q", f.Severity, "info")
	}
	if f.RecommendedSQL != "" {
		t.Errorf("safe process should have no SQL, got %q", f.RecommendedSQL)
	}
	if f.Category != "lock_chain" {
		t.Errorf("category: got %q, want %q", f.Category, "lock_chain")
	}
	if !strings.Contains(f.Recommendation, "Safe process") {
		t.Errorf("recommendation should mention safe process: %q", f.Recommendation)
	}
}

func TestLockChainFindings_ActionableWarning(t *testing.T) {
	lcCfg := baseLockChainConfig()
	chains := []LockChain{
		{
			RootBlockerPID:   100,
			RootBlockerApp:   "my_app",
			RootBlockerState: "idle",
			RootBlockerSince: time.Now().Add(-2 * time.Minute),
			TotalBlocked:     5, // above min (3), below critical (10)
			BlockedPIDs:      []int{200, 201, 202, 203, 204},
			ChainDepth:       2,
		},
	}

	findings := lockChainFindings(chains, lcCfg, 1)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.Severity != "warning" {
		t.Errorf("severity: got %q, want %q", f.Severity, "warning")
	}
}

func TestLockChainFindings_ActionableCritical(t *testing.T) {
	lcCfg := baseLockChainConfig()
	chains := []LockChain{
		{
			RootBlockerPID:   100,
			RootBlockerApp:   "my_app",
			RootBlockerState: "idle",
			RootBlockerSince: time.Now().Add(-2 * time.Minute),
			TotalBlocked:     15, // above critical (10)
			BlockedPIDs:      make([]int, 15),
			ChainDepth:       3,
		},
	}

	findings := lockChainFindings(chains, lcCfg, 1)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.Severity != "critical" {
		t.Errorf("severity: got %q, want %q", f.Severity, "critical")
	}
}

func TestLockChainFindings_IdleInTxTerminate(t *testing.T) {
	lcCfg := baseLockChainConfig()
	chains := []LockChain{
		{
			RootBlockerPID:   100,
			RootBlockerApp:   "my_app",
			RootBlockerState: "idle in transaction",
			RootBlockerSince: time.Now().Add(-10 * time.Minute), // 10min > 5min threshold
			TotalBlocked:     5,
			BlockedPIDs:      []int{200, 201, 202, 203, 204},
			ChainDepth:       2,
		},
	}

	findings := lockChainFindings(chains, lcCfg, 1)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	wantSQL := "SELECT pg_terminate_backend(100);"
	if f.RecommendedSQL != wantSQL {
		t.Errorf("RecommendedSQL: got %q, want %q", f.RecommendedSQL, wantSQL)
	}
	if !strings.Contains(f.Recommendation, "idle in transaction") {
		t.Errorf("recommendation should mention idle in transaction: %q",
			f.Recommendation)
	}
	if !strings.Contains(f.Recommendation, "terminate") {
		t.Errorf("recommendation should mention terminate: %q", f.Recommendation)
	}
	if f.ActionRisk != "moderate" {
		t.Errorf("ActionRisk: got %q, want %q", f.ActionRisk, "moderate")
	}
}

func TestLockChainFindings_ActiveQueryCancel(t *testing.T) {
	lcCfg := baseLockChainConfig()
	chains := []LockChain{
		{
			RootBlockerPID:   200,
			RootBlockerApp:   "analytics_worker",
			RootBlockerState: "active",
			RootBlockerSince: time.Now().Add(-20 * time.Minute), // 20min > 15min threshold
			TotalBlocked:     4,
			BlockedPIDs:      []int{300, 301, 302, 303},
			ChainDepth:       1,
		},
	}

	findings := lockChainFindings(chains, lcCfg, 1)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	wantSQL := "SELECT pg_cancel_backend(200);"
	if f.RecommendedSQL != wantSQL {
		t.Errorf("RecommendedSQL: got %q, want %q", f.RecommendedSQL, wantSQL)
	}
	if !strings.Contains(f.Recommendation, "cancel") {
		t.Errorf("recommendation should mention cancel: %q", f.Recommendation)
	}
	if f.ActionRisk != "safe" {
		t.Errorf("ActionRisk: got %q, want %q", f.ActionRisk, "safe")
	}
}

func TestLockChainFindings_MonitoringState(t *testing.T) {
	lcCfg := baseLockChainConfig()
	chains := []LockChain{
		{
			RootBlockerPID:   300,
			RootBlockerApp:   "my_app",
			RootBlockerState: "idle",
			RootBlockerSince: time.Now().Add(-30 * time.Minute),
			TotalBlocked:     5,
			BlockedPIDs:      []int{400, 401, 402, 403, 404},
			ChainDepth:       1,
		},
	}

	findings := lockChainFindings(chains, lcCfg, 1)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}

	f := findings[0]
	if f.RecommendedSQL != "" {
		t.Errorf("monitoring state should have no SQL, got %q",
			f.RecommendedSQL)
	}
	if !strings.Contains(f.Recommendation, "monitoring") {
		t.Errorf("recommendation should mention monitoring: %q",
			f.Recommendation)
	}
	wantPID := fmt.Sprintf("PID %d", 300)
	if !strings.Contains(f.Recommendation, wantPID) {
		t.Errorf("recommendation should mention PID: %q", f.Recommendation)
	}
	if !strings.Contains(f.Recommendation, "'idle'") {
		t.Errorf("recommendation should mention state: %q", f.Recommendation)
	}
}

func TestLockChainFindings_EmptyChains(t *testing.T) {
	lcCfg := baseLockChainConfig()

	findings := lockChainFindings(nil, lcCfg, 1)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for nil chains, got %d", len(findings))
	}

	findings = lockChainFindings([]LockChain{}, lcCfg, 1)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty chains, got %d", len(findings))
	}
}

// ---------------------------------------------------------------------------
// buildSafeFinding
// ---------------------------------------------------------------------------

func TestBuildSafeFinding_Fields(t *testing.T) {
	now := time.Now().Add(-5 * time.Minute)
	c := LockChain{
		RootBlockerPID:   42,
		RootBlockerQuery: "SELECT pg_advisory_lock(1)",
		RootBlockerState: "active",
		RootBlockerApp:   "patroni",
		RootBlockerSince: now,
		LockedRelation:   "public.orders",
		BlockerMode:      "AccessExclusiveLock",
		ChainDepth:       3,
		BlockedPIDs:      []int{100, 101, 102},
		TotalBlocked:     3,
	}

	f := buildSafeFinding(c)

	if f.Category != "lock_chain" {
		t.Errorf("Category: got %q, want %q", f.Category, "lock_chain")
	}
	if f.Severity != "info" {
		t.Errorf("Severity: got %q, want %q", f.Severity, "info")
	}
	if f.ObjectType != "lock" {
		t.Errorf("ObjectType: got %q, want %q", f.ObjectType, "lock")
	}
	if f.ObjectIdentifier != "pid:42" {
		t.Errorf("ObjectIdentifier: got %q, want %q",
			f.ObjectIdentifier, "pid:42")
	}
	if !strings.Contains(f.Title, "safe process") {
		t.Errorf("Title should contain 'safe process': %q", f.Title)
	}
	if !strings.Contains(f.Title, "PID 42") {
		t.Errorf("Title should contain 'PID 42': %q", f.Title)
	}
	if !strings.Contains(f.Title, "patroni") {
		t.Errorf("Title should contain app name: %q", f.Title)
	}
	if !strings.Contains(f.Title, "3 sessions") {
		t.Errorf("Title should contain blocked count: %q", f.Title)
	}
	if !strings.Contains(f.Recommendation, "Safe process") {
		t.Errorf("Recommendation: got %q", f.Recommendation)
	}
	if f.RecommendedSQL != "" {
		t.Errorf("RecommendedSQL should be empty for safe: %q",
			f.RecommendedSQL)
	}
	if f.Detail == nil {
		t.Fatal("Detail map should not be nil")
	}
}

// ---------------------------------------------------------------------------
// buildActionableFinding
// ---------------------------------------------------------------------------

func TestBuildActionableFinding_SeverityEscalation(t *testing.T) {
	lcCfg := baseLockChainConfig()

	tests := []struct {
		name         string
		totalBlocked int
		wantSeverity string
	}{
		{
			name:         "below critical threshold is warning",
			totalBlocked: 5,
			wantSeverity: "warning",
		},
		{
			name:         "at critical threshold is critical",
			totalBlocked: 10,
			wantSeverity: "critical",
		},
		{
			name:         "above critical threshold is critical",
			totalBlocked: 25,
			wantSeverity: "critical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := LockChain{
				RootBlockerPID:   500,
				RootBlockerApp:   "my_app",
				RootBlockerState: "idle",
				RootBlockerSince: time.Now().Add(-1 * time.Minute),
				TotalBlocked:     tt.totalBlocked,
				BlockedPIDs:      make([]int, tt.totalBlocked),
				ChainDepth:       2,
			}
			f := buildActionableFinding(c, lcCfg)
			if f.Severity != tt.wantSeverity {
				t.Errorf("severity: got %q, want %q",
					f.Severity, tt.wantSeverity)
			}
			// Verify common fields are populated regardless of severity.
			if f.Category != "lock_chain" {
				t.Errorf("Category: got %q, want %q",
					f.Category, "lock_chain")
			}
			if f.ObjectType != "lock" {
				t.Errorf("ObjectType: got %q, want %q",
					f.ObjectType, "lock")
			}
			if f.ObjectIdentifier != "pid:500" {
				t.Errorf("ObjectIdentifier: got %q, want %q",
					f.ObjectIdentifier, "pid:500")
			}
			if f.Title == "" {
				t.Error("Title should not be empty")
			}
			if f.Recommendation == "" {
				t.Error("Recommendation should not be empty")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// lockChainDetail
// ---------------------------------------------------------------------------

func TestLockChainDetail_AllFields(t *testing.T) {
	now := time.Now().Add(-7 * time.Minute)
	longQuery := strings.Repeat("SELECT 1; ", 50) // 500 chars, will truncate
	c := LockChain{
		RootBlockerPID:   77,
		RootBlockerQuery: longQuery,
		RootBlockerState: "idle in transaction",
		RootBlockerApp:   "django",
		RootBlockerSince: now,
		LockedRelation:   "public.accounts",
		BlockerMode:      "RowExclusiveLock",
		ChainDepth:       4,
		BlockedPIDs:      []int{80, 81, 82},
		TotalBlocked:     3,
	}

	d := lockChainDetail(c)

	requiredKeys := []string{
		"root_blocker_pid",
		"root_blocker_query",
		"root_blocker_state",
		"root_blocker_app",
		"root_blocker_since",
		"locked_relation",
		"blocker_mode",
		"chain_depth",
		"blocked_pids",
		"total_blocked",
	}
	for _, key := range requiredKeys {
		if _, ok := d[key]; !ok {
			t.Errorf("missing key %q in detail map", key)
		}
	}

	// Verify specific values.
	if pid, ok := d["root_blocker_pid"].(int); !ok || pid != 77 {
		t.Errorf("root_blocker_pid: got %v, want 77", d["root_blocker_pid"])
	}
	if state, ok := d["root_blocker_state"].(string); !ok || state != "idle in transaction" {
		t.Errorf("root_blocker_state: got %v, want 'idle in transaction'",
			d["root_blocker_state"])
	}
	if app, ok := d["root_blocker_app"].(string); !ok || app != "django" {
		t.Errorf("root_blocker_app: got %v, want 'django'",
			d["root_blocker_app"])
	}
	if since, ok := d["root_blocker_since"].(time.Time); !ok || !since.Equal(now) {
		t.Errorf("root_blocker_since: got %v, want %v",
			d["root_blocker_since"], now)
	}
	if rel, ok := d["locked_relation"].(string); !ok || rel != "public.accounts" {
		t.Errorf("locked_relation: got %v, want 'public.accounts'",
			d["locked_relation"])
	}
	if mode, ok := d["blocker_mode"].(string); !ok || mode != "RowExclusiveLock" {
		t.Errorf("blocker_mode: got %v, want 'RowExclusiveLock'",
			d["blocker_mode"])
	}
	if depth, ok := d["chain_depth"].(int); !ok || depth != 4 {
		t.Errorf("chain_depth: got %v, want 4", d["chain_depth"])
	}
	if blocked, ok := d["total_blocked"].(int); !ok || blocked != 3 {
		t.Errorf("total_blocked: got %v, want 3", d["total_blocked"])
	}

	// Verify query was truncated to 200 chars + "...".
	queryVal, ok := d["root_blocker_query"].(string)
	if !ok {
		t.Fatal("root_blocker_query should be a string")
	}
	if len(queryVal) != 203 {
		t.Errorf("root_blocker_query length: got %d, want 203", len(queryVal))
	}
	if !strings.HasSuffix(queryVal, "...") {
		t.Error("root_blocker_query should be truncated with '...'")
	}

	// Verify blocked_pids slice.
	pids, ok := d["blocked_pids"].([]int)
	if !ok {
		t.Fatal("blocked_pids should be []int")
	}
	if len(pids) != 3 {
		t.Errorf("blocked_pids length: got %d, want 3", len(pids))
	}
	wantPIDs := []int{80, 81, 82}
	for i, want := range wantPIDs {
		if pids[i] != want {
			t.Errorf("blocked_pids[%d]: got %d, want %d", i, pids[i], want)
		}
	}
}
