package executor

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests here: these are pure parsers and nil guards;
// the concurrent reconcile is covered in rollback_reconcile_db_test.go.

func resumeExecutorConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	return cfg
}

func TestReconcileStuck_NilPoolIsNoOp(t *testing.T) {
	exec := New(nil, resumeExecutorConfig(), time.Time{}, nopLog)
	n, err := exec.reconcileStuckRollbacks(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("nil pool: settled %d, err %v; want 0, nil", n, err)
	}
	ok, err := exec.reconcileStuckRollback(context.Background(), 1)
	if err != nil || ok {
		t.Fatalf("nil pool: settled %v, err %v; want false, nil", ok, err)
	}
}

func TestReconcileStuck_GraceWithoutConfigUsesDefault(t *testing.T) {
	exec := New(nil, nil, time.Time{}, nopLog)
	want := time.Duration(config.DefaultDDLTimeoutSeconds)*time.Second + time.Minute
	if got := exec.stuckRollbackGrace(); got != want {
		t.Fatalf("grace = %v, want %v", got, want)
	}
	exec = New(nil, resumeExecutorConfig(), time.Time{}, nopLog)
	exec.cfg.Safety.DDLTimeoutSeconds = 0
	if got := exec.stuckRollbackGrace(); got != want {
		t.Fatalf("zero ddl timeout: grace = %v, want the default %v", got, want)
	}
}

func TestRecordActionOutcome_NilPoolIsAnError(t *testing.T) {
	logs := &outcomeLog{}
	if err := recordActionOutcome(context.Background(), nil, logs.log, 7, "rolled_back",
		"x"); err == nil {
		t.Fatal("a write without a pool must fail")
	}
	if !logs.contains("7", "rolled_back") {
		t.Fatalf("log = %v, want the action id and outcome", logs.lines)
	}
}

func TestDroppedIndexTarget(t *testing.T) {
	cases := []struct {
		sql, want string
		ok        bool
	}{
		{"DROP INDEX CONCURRENTLY IF EXISTS public.idx_a", "public.idx_a", true},
		{"drop index concurrently public.\"Idx B\";", "public.\"Idx B\"", true},
		{"DROP INDEX idx_c", "idx_c", true},
		{"DROP INDEX IF EXISTS s.idx_d RESTRICT", "s.idx_d", true},
		{"DROP INDEX a, b", "", false},
		{"DROP INDEX idx_e CASCADE", "", false},
		{"DROP TABLE public.t", "", false},
		{"", "", false},
		{"CREATE INDEX idx_f ON public.t (a)", "", false},
	}
	for _, c := range cases {
		got, ok := droppedIndexTarget(c.sql)
		if got != c.want || ok != c.ok {
			t.Errorf("droppedIndexTarget(%q) = %q, %v; want %q, %v",
				c.sql, got, ok, c.want, c.ok)
		}
	}
}
