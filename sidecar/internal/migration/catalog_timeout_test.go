package migration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// static.md F11: the DDL risk assessment read table stats, activity,
// locks and replication lag with no server timeout. It now reads through
// catalogread under safety.query_timeout_ms; a slow read is cut off and
// the risk is scored from what is known (the intrinsic hazard).

type riskLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *riskLog) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func TestAssess_SlowReadsCutOffAndScoreIntrinsicHazard(t *testing.T) {
	pool, ctx := requireDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.risk_bounded")
	})
	if _, err := pool.Exec(ctx, `CREATE TABLE public.risk_bounded (id int);
		INSERT INTO public.risk_bounded SELECT generate_series(1, 1000);
		ANALYZE public.risk_bounded`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	logs := &riskLog{}
	ra := NewRiskAssessor(pool, logs.log)
	ra.SetCatalogReadTimeouts(catalogread.Timeouts{Statement: 200 * time.Millisecond})
	c := DDLClassification{Statement: "ALTER TABLE public.risk_bounded ADD COLUMN x int",
		LockLevel: "ACCESS EXCLUSIVE", TableName: "risk_bounded", SchemaName: "public"}
	base, err := ra.Assess(ctx, c)
	if err != nil || base.EstimatedRows != 1000 {
		t.Fatalf("baseline risk = %+v (%v), want the table's 1,000 rows", base, err)
	}
	var calls atomic.Int32
	slow := catalogread.WithBeforeStatement(ctx, func(ctx context.Context, tx pgx.Tx) error {
		calls.Add(1)
		_, err := tx.Exec(ctx, "SELECT pg_sleep(10)")
		return err
	})
	start := time.Now()
	risk, err := ra.Assess(slow, c)
	if err != nil || risk == nil || risk.EstimatedRows != 0 || risk.RiskScore <= 0 {
		t.Fatalf("risk under slow reads = %+v (%v), want the intrinsic score only", risk, err)
	}
	if calls.Load() != 4 {
		t.Fatalf("%d reads went through the bounded helper, want 4", calls.Load())
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("assessment under slow reads took %s", el)
	}
	timeouts := 0
	for _, l := range logs.lines {
		if strings.Contains(l, "statement timeout") {
			timeouts++
		}
	}
	if timeouts != 4 {
		t.Fatalf("%d logged statement timeouts, want 4: %v", timeouts, logs.lines)
	}
}

func TestRiskAssessor_CatalogReadTimeouts(t *testing.T) {
	ra := NewRiskAssessor(nil, func(string, string, ...any) {})
	if ra.timeouts != catalogread.Default() {
		t.Fatalf("default timeouts = %+v", ra.timeouts)
	}
	want := catalogread.Timeouts{Statement: time.Second, Lock: time.Second}
	adv := NewAdvisor(nil, &config.MigrationConfig{}, 170000, "db",
		func(string, string, ...any) {}, nil).WithCatalogReadTimeouts(want)
	if adv.assessor.timeouts != want {
		t.Fatalf("advisor did not pass timeouts: %+v", adv.assessor.timeouts)
	}
}
