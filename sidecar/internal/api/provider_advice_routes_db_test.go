package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/provideradvice"
)

// market-09: GET /api/v1/provider-advice?database= lists the provider's
// own advice with pg_sage's verdict and each source's last fetch, and a
// finding case carries the advice behind its finding.
//
// No concurrent access tests: each request is one read with no shared
// state; concurrent writes are tested in internal/provideradvice.

func cleanProviderAdvice(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		for _, sql := range []string{"DELETE FROM sage.provider_recommendations",
			"DELETE FROM sage.provider_advice_sources"} {
			if _, err := pool.Exec(context.Background(), sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
}

func seedAdvice(t *testing.T, pool *pgxpool.Pool, database string, finding int64) {
	t.Helper()
	ctx := context.Background()
	store := provideradvice.NewStore(pool)
	rows := []provideradvice.Row{
		{ProviderID: "p1", Kind: provideradvice.KindCreateIndex,
			Verdict: provideradvice.VerdictAgree, Reason: "pg_sage proposes it",
			FindingID: finding, Payload: provideradvice.Payload{Object: "public.users",
				DDL: "CREATE INDEX ON public.users (email)"}},
		{ProviderID: "p2", Kind: provideradvice.KindCreateIndex,
			Verdict: provideradvice.VerdictUnparsed, Reason: "not offered as SQL",
			Payload: provideradvice.Payload{ProviderText: "CREATE INDEX ON t ((("}},
	}
	if err := store.Apply(ctx, database, "cloud-sql", rows); err != nil {
		t.Fatalf("seed advice: %v", err)
	}
	if err := store.RecordSourceError(ctx, database, "azure",
		errors.New("azure index tuning HTTP 403")); err != nil {
		t.Fatalf("seed source error: %v", err)
	}
}

func TestProviderAdviceHandler_ListsAdviceAndSources(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanProviderAdvice(t, pool)
	seedAdvice(t, pool, "testdb", 0)
	mgr := phase2MgrWithPool(pool)
	code, body := serveManaged(t, providerAdviceHandler(mgr), "GET",
		"/api/v1/provider-advice?database=testdb", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 || body["meaning"] == "" {
		t.Fatalf("list = %d %v", code, body)
	}
	db, _ := dbs[0].(map[string]any)
	advice, _ := db["advice"].([]any)
	sources, _ := db["sources"].([]any)
	if db["database"] != "testdb" || len(advice) != 2 || len(sources) != 2 {
		t.Fatalf("database entry = %v", db)
	}
	agree, _ := advice[0].(map[string]any)
	unparsed, _ := advice[1].(map[string]any)
	ap, _ := agree["payload"].(map[string]any)
	up, _ := unparsed["payload"].(map[string]any)
	if agree["verdict"] != "agree" || ap["ddl"] != "CREATE INDEX ON public.users (email)" {
		t.Fatalf("agree = %v", agree)
	}
	if _, hasDDL := up["ddl"]; hasDDL || unparsed["verdict"] != "unparsed" ||
		up["provider_text"] != "CREATE INDEX ON t (((" {
		t.Fatalf("unparsed advice must carry no ddl: %v", unparsed)
	}
	azure, _ := sources[0].(map[string]any)
	if azure["provider"] != "azure" || azure["last_error"] != "azure index tuning HTTP 403" {
		t.Fatalf("sources = %v", sources)
	}
}

func TestProviderAdviceHandler_Validation(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cleanProviderAdvice(t, pool)
	mgr := phase2MgrWithPool(pool)
	h := providerAdviceHandler(mgr)
	for target, want := range map[string]int{
		"/api/v1/provider-advice?database=nope":                      http.StatusNotFound,
		"/api/v1/provider-advice?database=testdb&include_resolved=x": http.StatusBadRequest,
		"/api/v1/provider-advice?database=bad%00name":                http.StatusBadRequest,
	} {
		if code, body := serveManaged(t, h, "GET", target, nil, 0); code != want {
			t.Fatalf("%s = %d %v, want %d", target, code, body, want)
		}
	}
	code, body := serveManaged(t, h, "GET",
		"/api/v1/provider-advice?database=testdb&include_resolved=true", nil, 0)
	dbs, _ := body["databases"].([]any)
	if code != http.StatusOK || len(dbs) != 1 {
		t.Fatalf("empty = %d %v", code, body)
	}
	db, _ := dbs[0].(map[string]any)
	if advice, ok := db["advice"].([]any); !ok || len(advice) != 0 {
		t.Fatalf("no advice must be an empty list, got %v", db["advice"])
	}
}

func TestFindingCases_ShowProviderAdvice(t *testing.T) {
	pool, _, ctx := tracedCasePool(t)
	resetRecommendationCases(t, ctx)
	cleanProviderAdvice(t, pool)
	withAdvice := seedRecHead(t, ctx, recHead{object: "public.pa_with_advice"})
	seedRecHead(t, ctx, recHead{object: "public.pa_without_advice"})
	seedAdvice(t, pool, "prod", withAdvice)
	mgr := dbCaseManager(t, ctx, pool)
	list, err := queryProjectedCases(ctx, mgr, "prod")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	got := casesByFinding(list)
	c, ok := got["public.pa_with_advice"]
	if !ok || len(c.ProviderAdvice) != 1 {
		t.Fatalf("case = %+v", c)
	}
	a := c.ProviderAdvice[0]
	if a.Provider != "cloud-sql" || a.Verdict != "agree" || a.Kind != "create_index" ||
		a.Object != "public.users" || a.Reason != "pg_sage proposes it" || a.LastSeen.IsZero() {
		t.Fatalf("advice = %+v", a)
	}
	if other := got["public.pa_without_advice"]; len(other.ProviderAdvice) != 0 ||
		other.ProviderAdviceUnavailable {
		t.Fatalf("a finding without advice shows none: %+v", other)
	}
}
