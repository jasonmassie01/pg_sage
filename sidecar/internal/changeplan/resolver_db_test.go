package changeplan

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// plan-01a: the database resolver finds findings in sage.findings,
// evidence ids in a stored pack (evid-02a's workpack.Resolve) and target
// calls in the hourly rollup of the last 14 days. No concurrent test:
// every lookup is one read; the store tests cover concurrent writers.

func TestDBResolver_Findings(t *testing.T) {
	pool := planPool(t, "plan_resolver_findings")
	ctx := context.Background()
	r := NewDBResolver(pool, pool)
	var finding int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail)
		VALUES ('index', 'warning', 'table', 'public.orders', 't', '{}') RETURNING id`).
		Scan(&finding); err != nil {
		t.Fatalf("seed a finding: %v", err)
	}
	if ok, err := r.FindingExists(ctx, finding); err != nil || !ok {
		t.Fatalf("existing finding: %v %v", ok, err)
	}
	if ok, err := r.FindingExists(ctx, finding+1000); err != nil || ok {
		t.Fatalf("missing finding: %v %v", ok, err)
	}
}

func TestDBResolver_EvidenceInAStoredPack(t *testing.T) {
	pool := planPool(t, "plan_resolver_packs")
	ctx := context.Background()
	r := NewDBResolver(pool, pool)
	now := time.Now().UTC()
	pack, err := workpack.Build(ctx, pool, workpack.Request{Database: "plan_resolver",
		Window: workpack.DefaultWindowEnding(now), Now: now})
	if err != nil {
		t.Fatalf("build a pack: %v", err)
	}
	if _, err := workpack.Save(ctx, pool, pack, now, "test"); err != nil {
		t.Fatalf("save the pack: %v", err)
	}
	ids := pack.IDs()
	if len(ids) == 0 {
		t.Fatal("the pack holds no evidence ids")
	}
	if ok, err := r.EvidenceExists(ctx, pack.ContentSHA256, ids[0]); err != nil || !ok {
		t.Fatalf("evidence %q in the stored pack: %v %v", ids[0], ok, err)
	}
	for _, id := range []string{"setting/not_a_setting_at_all", "../etc", "", "bogus"} {
		if ok, err := r.EvidenceExists(ctx, pack.ContentSHA256, id); err != nil || ok {
			t.Errorf("evidence %q: %v %v, want false and no error", id, ok, err)
		}
	}
	if ok, err := r.EvidenceExists(ctx, strings.Repeat("cd", 32), ids[0]); err != nil || ok {
		t.Fatalf("a pack that is not stored: %v %v, want false and no error", ok, err)
	}
	if ok, err := r.EvidenceExists(ctx, "", ids[0]); err != nil || ok {
		t.Fatalf("no pack: %v %v, want false", ok, err)
	}
}

func TestDBResolver_TargetCallsInTheLast14Days(t *testing.T) {
	pool := planPool(t, "plan_resolver_calls")
	ctx := context.Background()
	r := NewDBResolver(pool, pool)
	now := time.Now().UTC()
	for _, row := range []struct {
		qid   int64
		calls int64
		ago   time.Duration
	}{{101, 40, 2 * time.Hour}, {101, 60, 13 * 24 * time.Hour}, {101, 1000, 15 * 24 * time.Hour},
		{102, 7, 3 * time.Hour}, {103, 9, time.Hour}} {
		hour := now.Add(-row.ago).Truncate(time.Hour)
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (queryid, hour, calls,
			exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
			temp_blks_written, epoch_break, pairs)
			VALUES ($1, $2, $3, 1, 1, 1, 1, 0, 0, false, 2)`, row.qid, hour,
			row.calls); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}
	got, err := r.TargetCalls(ctx, []int64{101, 102}, EvidenceWindow)
	if err != nil || got != 107 {
		t.Fatalf("calls of 101 and 102 in 14 days: %d %v, want 107 (the 15-day row is out)",
			got, err)
	}
	if got, err := r.TargetCalls(ctx, nil, EvidenceWindow); err != nil || got != 0 {
		t.Fatalf("no targets: %d %v", got, err)
	}
	if got, err := r.TargetCalls(ctx, []int64{999}, EvidenceWindow); err != nil || got != 0 {
		t.Fatalf("unknown target: %d %v", got, err)
	}
}

func TestDBResolver_ErrorsNameTheLookup(t *testing.T) {
	pool := planPool(t, "plan_resolver_err")
	r := NewDBResolver(pool, pool)
	pool.Close()
	ctx := context.Background()
	if _, err := r.FindingExists(ctx, 1); err == nil ||
		!strings.Contains(err.Error(), "finding 1") {
		t.Fatalf("finding on a closed pool: %v", err)
	}
	if _, err := r.TargetCalls(ctx, []int64{5}, EvidenceWindow); err == nil ||
		!strings.Contains(err.Error(), "calls") {
		t.Fatalf("calls on a closed pool: %v", err)
	}
	if _, err := r.EvidenceExists(ctx, strings.Repeat("ab", 32), "setting/x"); err == nil ||
		!strings.Contains(err.Error(), "pack") {
		t.Fatalf("evidence on a closed pool: %v", err)
	}
}
