package ask

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/config"
)

// mcp-07: ask.max_seconds_per_question is the question's wall clock; the
// per-step bound stays 15 s and the caller's deadline still clamps it.
// No concurrent access tests: New and runBudget read immutable inputs.

func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("lazy pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestNew_WallComesFromMaxSecondsPerQuestion(t *testing.T) {
	for seconds, want := range map[int]time.Duration{40: 40 * time.Second,
		15: 15 * time.Second, 300: 300 * time.Second, 0: 60 * time.Second} {
		cfg := config.AskConfig{Enabled: true, MaxSecondsPerQuestion: seconds,
			MaxTokensPerQuestion: 8000}
		s, err := New(Deps{Database: "db", Pool: lazyPool(t), Config: cfg})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if s.d.Budget.Wall != want || s.d.Budget.StepTimeout != 15*time.Second ||
			s.d.Budget.MaxTokens != 8000 || s.d.Budget.MaxSteps != 6 {
			t.Errorf("max_seconds_per_question %d: budget = %+v, want wall %s", seconds,
				s.d.Budget, want)
		}
	}
}

func TestNew_ExplicitBudgetIsKept(t *testing.T) {
	b := agentloop.Budget{MaxSteps: 2, MaxCalls: 1, MaxCost: 1, Wall: time.Second,
		MaxTokens: 5000, StepTokens: 100, StepTimeout: time.Second}
	s, err := New(Deps{Database: "db", Pool: lazyPool(t), Budget: b,
		Config: config.AskConfig{MaxSecondsPerQuestion: 120}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.d.Budget != b {
		t.Fatalf("budget = %+v, want %+v", s.d.Budget, b)
	}
}

func TestDefaultBudget_UsesTheDefaultSetting(t *testing.T) {
	b := DefaultBudget()
	if b.Wall != time.Duration(config.DefaultAskMaxSecondsPerQuestion)*time.Second ||
		b.StepTimeout != 15*time.Second {
		t.Fatalf("DefaultBudget = %+v", b)
	}
}

func TestRunBudget_CallerDeadlineStillClamps(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s, err := New(Deps{Database: "db", Pool: lazyPool(t), Now: func() time.Time { return now },
		Config: config.AskConfig{MaxSecondsPerQuestion: 120}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(45*time.Second))
	defer cancel()
	if got := s.runBudget(ctx).Wall; got != 45*time.Second-deadlineMargin {
		t.Fatalf("clamped wall = %s, want %s", got, 45*time.Second-deadlineMargin)
	}
	if got := s.runBudget(context.Background()).Wall; got != 120*time.Second {
		t.Fatalf("unbounded wall = %s, want 120s", got)
	}
}
