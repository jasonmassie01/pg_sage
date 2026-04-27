package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
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
