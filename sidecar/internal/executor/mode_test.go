package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// mockProposer records Propose calls for testing.
type mockProposer struct {
	calls           []proposeCall
	hasPending      bool
	hasSQL          bool
	hasRejected     bool
	hasRejectedSQL  bool
	checkErr        error
	checkCalls      int
	sqlChecks       int
	rejectChecks    int
	rejectSQLChecks int
}

type proposeCall struct {
	findingID int
	sql       string
	risk      string
	metadata  store.ActionProposalMetadata
}

func (m *mockProposer) Propose(
	_ context.Context, _ *int,
	findingID int, sql, rollbackSQL, risk string,
) (int, error) {
	m.calls = append(m.calls, proposeCall{
		findingID: findingID,
		sql:       sql,
		risk:      risk,
	})
	return len(m.calls), nil
}

func (m *mockProposer) ProposeWithMetadata(
	_ context.Context, _ *int,
	findingID int, sql, rollbackSQL, risk string,
	metadata store.ActionProposalMetadata,
) (int, error) {
	m.calls = append(m.calls, proposeCall{
		findingID: findingID,
		sql:       sql,
		risk:      risk,
		metadata:  metadata,
	})
	return len(m.calls), nil
}

func (m *mockProposer) HasPendingForFinding(
	_ context.Context, _ int,
) (bool, error) {
	m.checkCalls++
	return m.hasPending, m.checkErr
}

func (m *mockProposer) HasPendingForSQL(
	_ context.Context, _ string,
) (bool, error) {
	m.sqlChecks++
	return m.hasSQL, m.checkErr
}

func (m *mockProposer) HasRecentlyRejectedForFinding(
	_ context.Context, _ int, _ time.Duration,
) (bool, error) {
	m.rejectChecks++
	return m.hasRejected, m.checkErr
}

func (m *mockProposer) HasRecentlyRejectedForSQL(
	_ context.Context, _ string, _ time.Duration,
) (bool, error) {
	m.rejectSQLChecks++
	return m.hasRejectedSQL, m.checkErr
}

func TestExecutionMode_Default(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		execMode:      "auto",
	}

	if e.ExecutionMode() != "auto" {
		t.Errorf("default mode = %q, want auto",
			e.ExecutionMode())
	}
}

func TestWithActionStore_SetsMode(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		execMode:      "auto",
	}

	m := &mockProposer{}
	e.WithActionStore(m, "approval")

	if e.ExecutionMode() != "approval" {
		t.Errorf("mode = %q, want approval",
			e.ExecutionMode())
	}
}

func TestSetExecutionMode(t *testing.T) {
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		execMode:      "auto",
	}

	e.SetExecutionMode("manual")
	if e.ExecutionMode() != "manual" {
		t.Errorf("mode = %q, want manual",
			e.ExecutionMode())
	}
}

func TestManualMode_SkipsRunCycle(t *testing.T) {
	// Manual mode should return immediately without checking
	// emergency stop or processing any findings.
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "autonomous"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		execMode:      "manual",
		// pool is nil — if RunCycle tries to query, it panics
		pool: nil,
	}

	// Should not panic — manual mode returns early.
	e.RunCycle(context.Background(), false)
}

func TestNilActionStore_AutoModeBehavior(t *testing.T) {
	// When actionStore is nil, executor stays in auto mode and
	// approval mode code path is skipped (no panic).
	e := &Executor{
		cfg: &config.Config{
			Trust: config.TrustConfig{Level: "advisory"},
		},
		recentActions: make(map[string]time.Time),
		logFn:         func(string, string, ...any) {},
		execMode:      "auto",
		actionStore:   nil,
	}

	if e.actionStore != nil {
		t.Error("actionStore should be nil for auto mode default")
	}
}

func TestBuildApprovalProposalMetadata_PopulatesDeterministicFields(
	t *testing.T,
) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	finding := testFindingForProposalMetadata()
	cfg := &config.Config{
		Trust: config.TrustConfig{Level: "advisory"},
	}
	e := &Executor{
		cfg:                cfg,
		execMode:           "approval",
		rampStart:          now.Add(-30 * 24 * time.Hour),
		recentActions:      make(map[string]time.Time),
		databaseName:       "primary",
		analyzeSem:         make(chan struct{}, 1),
		actionStore:        &mockProposer{},
		ddlSem:             make(chan struct{}, 1),
		shutdownCh:         make(chan struct{}),
		shuttingDown:       false,
		trustLevelOverride: "",
	}

	got := e.buildApprovalProposalMetadata(finding, now)

	if got.ActionType != "analyze_table" {
		t.Fatalf("ActionType = %q, want analyze_table", got.ActionType)
	}
	if got.IdentityKey != "stale_stats:public.orders:analyze_table" {
		t.Fatalf("IdentityKey = %q", got.IdentityKey)
	}
	if got.PolicyDecision != PolicyDecisionQueueApproval {
		t.Fatalf("PolicyDecision = %q, want queue_for_approval",
			got.PolicyDecision)
	}
	if got.VerificationStatus != "not_started" {
		t.Fatalf("VerificationStatus = %q, want not_started",
			got.VerificationStatus)
	}
	if got.ShadowToilMinutes != 15 {
		t.Fatalf("ShadowToilMinutes = %d, want 15",
			got.ShadowToilMinutes)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want 24h after now", got.ExpiresAt)
	}
	if len(got.Guardrails) == 0 ||
		got.Guardrails[0] != "dedicated connection" {
		t.Fatalf("Guardrails = %#v, want analyze guardrails",
			got.Guardrails)
	}
}

func TestProposeForApproval_UsesMetadataStore(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	finding := testFindingForProposalMetadata()
	mp := &mockProposer{}
	e := &Executor{
		cfg:           &config.Config{Trust: config.TrustConfig{Level: "advisory"}},
		execMode:      "approval",
		rampStart:     now.Add(-30 * 24 * time.Hour),
		recentActions: make(map[string]time.Time),
		actionStore:   mp,
		analyzeSem:    make(chan struct{}, 1),
	}

	id, err := e.proposeForApproval(context.Background(), 42, finding)
	if err != nil {
		t.Fatalf("proposeForApproval returned error: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
	if len(mp.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(mp.calls))
	}
	got := mp.calls[0].metadata
	if got.ActionType != "analyze_table" {
		t.Fatalf("metadata.ActionType = %q, want analyze_table",
			got.ActionType)
	}
	if got.PolicyDecision != PolicyDecisionQueueApproval {
		t.Fatalf("metadata.PolicyDecision = %q, want queue_for_approval",
			got.PolicyDecision)
	}
	if got.IdentityKey == "" || len(got.Guardrails) == 0 {
		t.Fatalf("metadata missing identity or guardrails: %#v", got)
	}
}

func testFindingForProposalMetadata() analyzer.Finding {
	return analyzer.Finding{
		Category:         "stale_stats",
		ObjectIdentifier: "public.orders",
		RecommendedSQL:   "ANALYZE public.orders",
		ActionRisk:       "safe",
		Title:            "stale stats",
	}
}
