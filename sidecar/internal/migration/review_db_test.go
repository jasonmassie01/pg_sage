package migration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/config"
)

// No concurrent access tests here: TestReviewConcurrent covers parallel
// reviews; these pin real catalog behaviour.

func reviewFixture(t *testing.T) (context.Context, catalogread.Reader, *Advisor) {
	t.Helper()
	pool, ctx := requireDB(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS review_db CASCADE")
	})
	if _, err := pool.Exec(ctx, `DROP SCHEMA IF EXISTS review_db CASCADE;
		CREATE SCHEMA review_db;
		CREATE TABLE review_db.t (id int, v varchar(20), c int);
		INSERT INTO review_db.t SELECT i, i::text, i FROM generate_series(1, 20000) i;
		ANALYZE review_db.t`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	q := catalogread.New(pool, catalogread.Timeouts{Statement: 2 * time.Second,
		Lock: time.Second})
	adv := NewAdvisor(pool, &config.MigrationConfig{}, 170000, "db",
		func(string, string, ...any) {}, nil)
	return ctx, q, adv
}

// Integration: on real PostgreSQL, lint and the advisor agree on
// varchar -> text (no rewrite) and on varchar -> int (a rewrite).
func TestReviewDBAgreesWithAdvisorOnTypeChange(t *testing.T) {
	ctx, q, adv := reviewFixture(t)
	for sql, rewrite := range map[string]bool{
		"ALTER TABLE review_db.t ALTER COLUMN v TYPE text":                  false,
		"ALTER TABLE review_db.t ALTER COLUMN v TYPE int USING v::int":      true,
		"ALTER TABLE review_db.t ALTER COLUMN v TYPE character varying(40)": false,
	} {
		rep, err := Review(ctx, q, sql, ReviewOptions{})
		if err != nil {
			t.Fatalf("Review(%q): %v", sql, err)
		}
		h := onlyHazard(t, rep, "ddl_alter_type_rewrite")
		_, risk, err := adv.AnalyzeRisk(ctx, sql)
		if err != nil || risk == nil {
			t.Fatalf("advisor(%q): %v %v", sql, risk, err)
		}
		if h.RequiresRewrite != rewrite || risk.RequiresRewrite != rewrite {
			t.Errorf("%q: review rewrite=%v advisor rewrite=%v, want %v", sql,
				h.RequiresRewrite, risk.RequiresRewrite, rewrite)
		}
		if h.LiveRows != 20000 || !h.SizeKnown {
			t.Errorf("%q: live rows %d, want 20000", sql, h.LiveRows)
		}
	}
}

// Integration: a plain CREATE INDEX on an analyzed table above the
// threshold is risky; the same migration read statically is review.
func TestReviewDBCreateIndexRisky(t *testing.T) {
	ctx, q, _ := reviewFixture(t)
	sql := "CREATE INDEX x ON review_db.t (c)"
	rep, err := Review(ctx, q, sql, ReviewOptions{})
	if err != nil || rep.Verdict != VerdictRisky || rep.PGVersion < 140000 {
		t.Fatalf("live: %+v (%v), want risky", rep, err)
	}
	rep, err = Review(ctx, q, sql, ReviewOptions{Static: true})
	if err != nil || rep.Verdict != VerdictReview {
		t.Fatalf("static: %+v (%v), want review", rep, err)
	}
}

// Error propagation: a catalog read cut off by statement_timeout is
// ErrCatalogUnavailable, never a verdict.
func TestReviewDBCatalogTimeout(t *testing.T) {
	ctx, q, _ := reviewFixture(t)
	q.Timeouts = catalogread.Timeouts{Statement: 100 * time.Millisecond}
	slow := catalogread.WithBeforeStatement(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT pg_sleep(5)")
		return err
	})
	start := time.Now()
	_, err := Review(slow, q, "CREATE INDEX x ON review_db.t (c)", ReviewOptions{})
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("err %v, want ErrCatalogUnavailable", err)
	}
	if el := time.Since(start); el > 4*time.Second {
		t.Fatalf("the timed-out review took %s", el)
	}
}
