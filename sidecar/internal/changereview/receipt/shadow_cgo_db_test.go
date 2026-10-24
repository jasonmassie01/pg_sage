//go:build cgo

package receipt

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// The flag is computed when the review completes: the service calls
// AutoClear(cfg) (Deps.AutoClear) and stores shadow_auto_clear.

// fixedAnalyzer returns one statement on rv.rv_orders with its table
// evidence (1 GB).
type fixedAnalyzer struct {
	t   *testing.T
	sql string
}

func (a fixedAnalyzer) Analyze(_ context.Context, _ changereview.Input) (
	changereview.Analysis, error) {
	r := shadowReview(a.t, a.sql, gib, "table")
	return changereview.Analysis{ParseMode: changereview.ParseAST, Statements: r.Statements,
		Findings: []changereview.Finding{}, Evidence: r.Evidence,
		Expectations: changereview.Expectations{Relations: []string{"rv.rv_orders"}}}, nil
}

func TestCompletedReviewRecordsTheShadowFlag(t *testing.T) {
	f := newFixture(t)
	cases := map[string]bool{cic: true,
		"ALTER TABLE rv.rv_orders DROP COLUMN status": false}
	for sql, want := range cases {
		svc, err := changereview.New(changereview.Deps{Database: f.name, Pool: f.pool,
			Config: f.cfg, Analyzer: fixedAnalyzer{t: t, sql: sql},
			AutoClear: AutoClear(f.cfg), Logf: t.Logf})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Start(f.ctx); err != nil {
			t.Fatal(err)
		}
		r, _, err := svc.Submit(f.ctx, changereview.Caller{Actor: "mcp:token:ci",
			Source: changereview.SourceMCP}, changereview.Request{SQL: sql, WaitSeconds: 20})
		if err != nil {
			t.Fatalf("Submit %q: %v", sql, err)
		}
		if r.State != changereview.StateComplete {
			if r, err = svc.Wait(f.ctx, r.ID, 20*time.Second); err != nil {
				t.Fatal(err)
			}
		}
		if r.ShadowAutoClear == nil || *r.ShadowAutoClear != want {
			t.Fatalf("%q: shadow_auto_clear %v, want %v", sql, r.ShadowAutoClear, want)
		}
	}
}
