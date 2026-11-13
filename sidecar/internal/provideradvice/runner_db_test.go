package provideradvice

import (
	"context"
	"errors"
	"testing"
)

// End to end on real PostgreSQL: the fake Cloud SQL advisor view, a pg_sage
// finding for the same index, the database evaluator and the store.
func TestRunner_CloudSQLAdviceReconcilesIntoTheStore(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, fakeAdvisorView)
	fid := insertFinding(t, "missing_index", "public.users|email",
		"CREATE INDEX CONCURRENTLY ON public.users (email);", "open", `{"pa_test":1}`)
	src, err := NewCloudSQLSource("cloud-sql", pool)
	if err != nil {
		t.Fatal(err)
	}
	failing := &fakeSource{name: "azure", recs: []Recommendation{dropRec("z1", "public.gone_idx")}}
	store := NewStore(pool)
	r, err := NewRunner(RunnerOptions{Database: "app", Sources: []Source{src, failing},
		Sink: store, Findings: func(c context.Context) ([]Finding, error) {
			return LoadFindings(c, pool)
		}, Eval: NewDBEvaluator(pool, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	recs, err := store.List(ctx, "app", false, 50)
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for _, rec := range recs {
		verdicts[rec.Provider+"/"+rec.Payload.Object] = rec.Verdict
		if rec.Verdict == VerdictAgree && (rec.FindingID == nil || *rec.FindingID != fid) {
			t.Fatalf("agree row without its finding: %+v", rec)
		}
	}
	if verdicts["cloud-sql/public.users"] != VerdictAgree ||
		verdicts["cloud-sql/public.orders"] != VerdictProviderOnly ||
		verdicts["azure/public.gone_idx"] != VerdictProviderOnly {
		t.Fatalf("verdicts = %v (rows %+v)", verdicts, recs)
	}

	failing.recs, failing.err = nil, errors.New("index tuning HTTP 403 AuthorizationFailed")
	if err := r.RunOnce(ctx); err == nil {
		t.Fatal("the failing source must surface")
	}
	after, _ := store.List(ctx, "app", false, 50)
	if len(after) != len(recs) {
		t.Fatalf("a source error changed the advice: %d -> %d rows", len(recs), len(after))
	}
	if st, _ := store.Sources(ctx, "app"); len(st) != 2 || st[0].LastError == "" {
		t.Fatalf("sources = %+v; azure must carry its error", st)
	}
}
