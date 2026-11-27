package provideradvice

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func agreeRow(id string, finding int64) Row {
	return Row{ProviderID: id, Kind: KindCreateIndex, Verdict: VerdictAgree,
		Reason: "pg_sage proposes an index that serves this", FindingID: finding,
		Payload: Payload{Object: "public.users", DDL: "CREATE INDEX ON public.users (email)",
			ObservedAt: seen}}
}

func providerOnly(id string) Row {
	return Row{ProviderID: id, Kind: KindCreateIndex, Verdict: VerdictProviderOnly,
		Reason:  "would not act: HypoPG is not installed",
		Payload: Payload{Object: "public.t", DDL: "CREATE INDEX ON public.t (a)"}}
}

func TestStore_ApplyRecordsAndRefreshes(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rows := []Row{agreeRow("p1", 0), providerOnly("p2")}
	if err := s.Apply(ctx, "app", "cloud-sql", rows); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	first, err := s.List(ctx, "app", false, 50)
	if err != nil || len(first) != 2 {
		t.Fatalf("List: %+v, %v", first, err)
	}
	time.Sleep(20 * time.Millisecond)
	rows = []Row{agreeRow("p1", 0), providerOnly("p2")}
	if err := s.Apply(ctx, "app", "cloud-sql", rows); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	again, err := s.List(ctx, "app", false, 50)
	if err != nil || len(again) != 2 {
		t.Fatalf("List again: %+v, %v", again, err)
	}
	for i := range again {
		if again[i].ID != first[i].ID || !again[i].FirstSeen.Equal(first[i].FirstSeen) ||
			!again[i].LastSeen.After(first[i].LastSeen) || again[i].ResolvedAt != nil {
			t.Fatalf("row %d: first %+v, again %+v", i, first[i], again[i])
		}
	}
	p1 := again[0]
	if p1.ProviderID != "p1" || p1.Verdict != VerdictAgree || p1.Database != "app" ||
		p1.Provider != "cloud-sql" || p1.Payload.DDL == "" || p1.FindingID != nil {
		t.Fatalf("p1 = %+v", p1)
	}
}

func TestStore_DisappearedAdviceIsResolvedAndCanReturn(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rows := []Row{providerOnly("a"), providerOnly("b")}
	if err := s.Apply(ctx, "app", "azure", rows); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, "app", "azure", []Row{providerOnly("a")}); err != nil {
		t.Fatal(err)
	}
	open, _ := s.List(ctx, "app", false, 50)
	all, _ := s.List(ctx, "app", true, 50)
	if len(open) != 1 || open[0].ProviderID != "a" || len(all) != 2 {
		t.Fatalf("open %+v, all %+v", open, all)
	}
	var gone Record
	for _, r := range all {
		if r.ProviderID == "b" {
			gone = r
		}
	}
	if gone.ResolvedAt == nil {
		t.Fatalf("b was not resolved: %+v", gone)
	}
	rows = []Row{providerOnly("a"), providerOnly("b")}
	if err := s.Apply(ctx, "app", "azure", rows); err != nil {
		t.Fatal(err)
	}
	if open, _ = s.List(ctx, "app", false, 50); len(open) != 2 {
		t.Fatalf("b came back but is still resolved: %+v", open)
	}
	if err := s.Apply(ctx, "app", "azure", nil); err != nil {
		t.Fatal(err)
	}
	if open, _ = s.List(ctx, "app", false, 50); len(open) != 0 {
		t.Fatalf("an empty list must resolve everything: %+v", open)
	}
}

func TestStore_ProvidersAndDatabasesAreIndependent(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	for _, w := range []struct{ db, provider string }{
		{"app", "azure"}, {"app", "supabase"}, {"billing", "azure"},
	} {
		if err := s.Apply(ctx, w.db, w.provider, []Row{providerOnly("x")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Apply(ctx, "app", "azure", nil); err != nil {
		t.Fatal(err)
	}
	app, _ := s.List(ctx, "app", false, 50)
	billing, _ := s.List(ctx, "billing", false, 50)
	if len(app) != 1 || app[0].Provider != "supabase" || len(billing) != 1 {
		t.Fatalf("app %+v, billing %+v", app, billing)
	}
}

func TestStore_SourceErrorLeavesRowsUnchanged(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	if err := s.Apply(ctx, "app", "azure", []Row{providerOnly("a")}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.List(ctx, "app", false, 50)
	fetchErr := errors.New("azure index tuning HTTP 403 AuthorizationFailed")
	if err := s.RecordSourceError(ctx, "app", "azure", fetchErr); err != nil {
		t.Fatalf("RecordSourceError: %v", err)
	}
	after, _ := s.List(ctx, "app", false, 50)
	if len(after) != 1 || !after[0].LastSeen.Equal(before[0].LastSeen) ||
		after[0].ResolvedAt != nil {
		t.Fatalf("a source error changed the rows: before %+v after %+v", before, after)
	}
	st, err := s.Sources(ctx, "app")
	if err != nil || len(st) != 1 || st[0].Provider != "azure" ||
		!strings.Contains(st[0].LastError, "403") || st[0].LastSuccess == nil ||
		st[0].LastAttempt.Before(*st[0].LastSuccess) {
		t.Fatalf("sources = %+v, %v", st, err)
	}
	if err := s.Apply(ctx, "app", "azure", []Row{providerOnly("a")}); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.Sources(ctx, "app"); st[0].LastError != "" {
		t.Fatalf("a successful fetch must clear the error: %+v", st[0])
	}
}

func TestStore_SourceErrorOnAFirstFetchHasNoSuccess(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	if err := s.RecordSourceError(ctx, "app", "supabase", errors.New("HTTP 401")); err != nil {
		t.Fatal(err)
	}
	st, err := s.Sources(ctx, "app")
	if err != nil || len(st) != 1 || st[0].LastSuccess != nil || st[0].LastError != "HTTP 401" {
		t.Fatalf("sources = %+v, %v", st, err)
	}
	if rows, _ := s.List(ctx, "app", true, 50); len(rows) != 0 {
		t.Fatalf("a source error wrote advice rows: %+v", rows)
	}
}

func TestStore_ConcurrentReconcilesWriteOneRowPerProviderID(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rows := []Row{agreeRow("p1", 0), providerOnly("p2"), providerOnly("p3")}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Apply(ctx, "app", "cloud-sql", rows)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Apply: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.provider_recommendations
		WHERE database = 'app' AND provider = 'cloud-sql'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows = %d (%v), want 3", n, err)
	}
}

func TestStore_ForFindingsGroupsOpenAdvice(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	rows := []Row{agreeRow("p1", 41), providerOnly("p2")}
	if err := s.Apply(ctx, "app", "cloud-sql", rows); err != nil {
		t.Fatal(err)
	}
	rows = []Row{agreeRow("z1", 41), agreeRow("z2", 42)}
	if err := s.Apply(ctx, "app", "azure", rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.ForFindings(ctx, "app", []int64{41, 99})
	if err != nil || len(got) != 1 || len(got[41]) != 2 {
		t.Fatalf("ForFindings = %+v, %v", got, err)
	}
	if empty, err := s.ForFindings(ctx, "app", nil); err != nil || len(empty) != 0 {
		t.Fatalf("no ids: %+v, %v", empty, err)
	}
}

func TestStore_RejectsInvalidInput(t *testing.T) {
	pool, ctx := testPool(t)
	s := NewStore(pool)
	cases := map[string]error{
		"no database": s.Apply(ctx, "", "azure", nil),
		"no provider": s.Apply(ctx, "app", "", nil),
		"no id":       s.Apply(ctx, "app", "azure", []Row{{Kind: KindCreateIndex}}),
		"bad verdict": s.Apply(ctx, "app", "azure", []Row{{ProviderID: "a",
			Kind: KindCreateIndex, Verdict: "maybe"}}),
		"error db": s.RecordSourceError(ctx, "", "azure", errors.New("x")),
		"nil err":  s.RecordSourceError(ctx, "app", "azure", nil),
	}
	for name, err := range cases {
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if rows, _ := s.List(ctx, "app", true, 50); len(rows) != 0 {
		t.Fatalf("rejected input wrote rows: %+v", rows)
	}
	if _, err := NewStore(nil).List(ctx, "app", false, 10); err == nil {
		t.Fatal("a nil pool must fail")
	}
}
