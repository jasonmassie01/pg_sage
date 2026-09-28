package analyzer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/recommendation"
)

func TestRecommendationProposalMapsFinding(t *testing.T) {
	f := Finding{
		Category: "missing_index", Severity: "critical", ObjectType: "table",
		ObjectIdentifier: "public.orders", Title: "index orders",
		Detail: map[string]any{"seq_scans": 7}, Recommendation: "add it",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY i ON public.orders (a)",
		RollbackSQL:    "DROP INDEX CONCURRENTLY i", ActionRisk: "safe",
		DatabaseName: "ignored",
	}
	p := RecommendationProposal("prod", f)
	if p.DatabaseName != "prod" || p.Category != f.Category ||
		p.Target != f.ObjectIdentifier || p.ObjectType != "table" ||
		p.Title != f.Title || p.Severity != "critical" || p.ActionRisk != "safe" ||
		p.Recommendation != "add it" || p.ForwardSQL != f.RecommendedSQL ||
		p.InverseSQL != f.RollbackSQL || p.Evidence["seq_scans"] != 7 {
		t.Fatalf("proposal = %+v", p)
	}
}

func recAnalyzer(t *testing.T) (*Analyzer, *pgxpool.Pool, *recommendation.Store) {
	t.Helper()
	a, pool, _ := lifecycleAnalyzer(t)
	a.WithDatabaseName(fmt.Sprintf("analyzer_rec_%d", time.Now().UnixNano()))
	return a, pool, recommendation.NewStore(pool)
}

func onlyRecommendation(
	t *testing.T, s *recommendation.Store, db string,
) recommendation.Recommendation {
	t.Helper()
	list, err := s.List(context.Background(), recommendation.ListFilter{DatabaseName: db})
	if err != nil || len(list) != 1 {
		t.Fatalf("recommendations for %s = %+v, %v; want exactly one", db, list, err)
	}
	return list[0]
}

// The analyzer writes a revision only when the content hash changes.
func TestFinalizeCycleWritesRevisionsOnContentChange(t *testing.T) {
	a, _, s := recAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_rec_revision", "public.orders", "warning")
	f.RollbackSQL = ""
	eval := map[string]bool{f.Category: true}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	f.Detail = map[string]any{"k": "changed evidence"}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	rec := onlyRecommendation(t, s, a.databaseName)
	if rec.Revision != 1 || rec.State != recommendation.StateProposed {
		t.Fatalf("after unchanged content: rev=%d state=%s, want 1 proposed",
			rec.Revision, rec.State)
	}
	f.RecommendedSQL = "VACUUM (ANALYZE) public.t;"
	a.finalizeCycle(ctx, []Finding{f}, eval)
	rec = onlyRecommendation(t, s, a.databaseName)
	revs, _ := s.Revisions(ctx, rec.ID)
	if rec.Revision != 2 || len(revs) != 2 || revs[1].ForwardSQL != f.RecommendedSQL {
		t.Fatalf("after new SQL: rev=%d revisions=%+v", rec.Revision, revs)
	}
}

func TestFinalizeCycleSkipsFindingsWithoutSQL(t *testing.T) {
	a, _, s := recAnalyzer(t)
	f := lifecycleFinding("test_rec_nosql", "public.orders", "info")
	f.RecommendedSQL = ""
	a.finalizeCycle(context.Background(), []Finding{f}, map[string]bool{f.Category: true})
	list, err := s.List(context.Background(),
		recommendation.ListFilter{DatabaseName: a.databaseName})
	if err != nil || len(list) != 0 {
		t.Fatalf("a finding without SQL became %d recommendations (%v)", len(list), err)
	}
}

// A candidate the analyzer stops emitting for an evaluated category is
// superseded; a category that was not evaluated is left alone.
func TestFinalizeCycleSupersedesAbsentCandidates(t *testing.T) {
	a, _, s := recAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_rec_absent", "public.orders", "warning")
	a.finalizeCycle(ctx, []Finding{f}, map[string]bool{f.Category: true})
	a.finalizeCycle(ctx, nil, map[string]bool{"some_other_category": true})
	if rec := onlyRecommendation(t, s, a.databaseName); rec.State !=
		recommendation.StateProposed {
		t.Fatalf("unevaluated category superseded its candidate: %s", rec.State)
	}
	a.finalizeCycle(ctx, nil, map[string]bool{f.Category: true})
	if rec := onlyRecommendation(t, s, a.databaseName); rec.State !=
		recommendation.StateSuperseded {
		t.Fatalf("absent candidate state = %s, want superseded", rec.State)
	}
}

func TestFinalizeCycleNeverProposesSuppressedIdentity(t *testing.T) {
	a, pool, s := recAnalyzer(t)
	ctx := context.Background()
	f := lifecycleFinding("test_rec_suppressed", "public.orders", "warning")
	eval := map[string]bool{f.Category: true}
	a.finalizeCycle(ctx, []Finding{f}, eval)
	suppressFinding(t, pool, f.Category, f.ObjectIdentifier)
	a.finalizeCycle(ctx, []Finding{f}, eval)
	if rec := onlyRecommendation(t, s, a.databaseName); rec.State !=
		recommendation.StateSuperseded {
		t.Fatalf("suppressed identity's recommendation = %s, want superseded", rec.State)
	}
}

func TestFinalizeCycleRecordsPolicyVersion(t *testing.T) {
	a, _, s := recAnalyzer(t)
	ctx := context.Background()
	a.WithPolicyVersion(func(context.Context) (int64, error) { return 12, nil })
	f := lifecycleFinding("test_rec_policy", "public.orders", "warning")
	a.finalizeCycle(ctx, []Finding{f}, map[string]bool{f.Category: true})
	rec := onlyRecommendation(t, s, a.databaseName)
	revs, _ := s.Revisions(ctx, rec.ID)
	if len(revs) != 1 || revs[0].PolicyVersion == nil || *revs[0].PolicyVersion != 12 {
		t.Fatalf("revision policy version = %+v", revs)
	}
	a.WithPolicyVersion(func(context.Context) (int64, error) {
		return 0, errors.New("policy store unavailable")
	})
	f.ObjectIdentifier = "public.items"
	a.finalizeCycle(ctx, []Finding{f}, map[string]bool{f.Category: true})
	list, _ := s.List(ctx, recommendation.ListFilter{DatabaseName: a.databaseName,
		State: recommendation.StateProposed})
	if len(list) != 1 {
		t.Fatalf("an unavailable policy version blocked the proposal: %d live", len(list))
	}
	revs, _ = s.Revisions(ctx, list[0].ID)
	if revs[0].PolicyVersion != nil {
		t.Fatalf("unknown policy version stored as %d, want NULL", *revs[0].PolicyVersion)
	}
}
