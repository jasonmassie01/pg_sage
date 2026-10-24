package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
)

// CHECK-UI-04 on real Postgres: GET /api/v1/cases lists every complete
// review with verdict review or risky and no DBA verdict as a
// change_review case (critical for risky, warning for review, why_now
// "waiting Nh for a DBA verdict"). A recorded verdict takes the review off
// the list; safe reviews never appear. Each database's reviews are read
// from its own pool only.

type crCasesFixture struct {
	mgr  *fleet.DatabaseManager
	pool *pgxpool.Pool
	svc  *changereview.Service
}

func newCRCasesFixture(t *testing.T) crCasesFixture {
	t.Helper()
	pool := crfake.Pool(t)
	svc := crfake.Start(t, pool, "db1", &crfake.Analyzer{}, nil)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "db1",
		Config: config.DatabaseConfig{Name: "db1"}, Pool: pool,
		Status: &fleet.InstanceStatus{Connected: true}})
	return crCasesFixture{mgr: mgr, pool: pool, svc: svc}
}

// submitCR runs one review to completion and returns it.
func submitCR(t *testing.T, svc *changereview.Service, sql string) changereview.Review {
	t.Helper()
	r, _, err := svc.Submit(context.Background(), changereview.Caller{Actor: "user:1",
		Source: changereview.SourceAPI}, changereview.Request{SQL: sql, WaitSeconds: 20,
		PR: changereview.PRInfo{URL: "https://github.com/acme/app/pull/7", Number: 7,
			Repo: "acme/app", Title: "PR for " + sql[:12]}})
	if err != nil || r.State != changereview.StateComplete {
		t.Fatalf("submit %q: state %q err %v", sql, r.State, err)
	}
	return r
}

func getCases(t *testing.T, mgr *fleet.DatabaseManager, query string) []cases.Case {
	t.Helper()
	rec := httptest.NewRecorder()
	casesHandler(mgr).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/cases"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cases%s: %d %s", query, rec.Code, rec.Body.String())
	}
	var body struct {
		Cases []cases.Case `json:"cases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var out []cases.Case
	for _, c := range body.Cases {
		if c.SourceType == cases.SourceChangeReviewType {
			out = append(out, c)
		}
	}
	return out
}

func caseByReview(list []cases.Case, id string) (cases.Case, bool) {
	for _, c := range list {
		if c.IdentityKey == "change_review:"+id {
			return c, true
		}
	}
	return cases.Case{}, false
}

func TestCasesListReviewsWaitingForADBA(t *testing.T) {
	f := newCRCasesFixture(t)
	risky := submitCR(t, f.svc, "ALTER TABLE orders DROP COLUMN email; "+crfake.MarkRisky)
	review := submitCR(t, f.svc, "ALTER TABLE orders ALTER c TYPE text "+crfake.MarkReview)
	safe := submitCR(t, f.svc, "CREATE TABLE orders_archive (id bigint primary key)")
	if risky.Verdict != "risky" || review.Verdict != "review" || safe.Verdict != "safe" {
		t.Fatalf("fixture verdicts %s/%s/%s", risky.Verdict, review.Verdict, safe.Verdict)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE sage.change_review
		SET completed_at = now() - interval '5 hours 10 minutes' WHERE id = $1`,
		risky.ID); err != nil {
		t.Fatal(err)
	}
	got := getCases(t, f.mgr, "")
	if len(got) != 2 {
		t.Fatalf("got %d change_review cases %+v, want 2 (safe never appears)", len(got), got)
	}
	rc, ok := caseByReview(got, risky.ID)
	if !ok || rc.Severity != cases.SeverityCritical || rc.DatabaseName != "db1" ||
		rc.WhyNow != "waiting 5h for a DBA verdict" || !strings.HasPrefix(rc.Why, "CR-03") {
		t.Fatalf("risky case %+v", rc)
	}
	wc, ok := caseByReview(got, review.ID)
	if !ok || wc.Severity != cases.SeverityWarning ||
		wc.WhyNow != "waiting 0h for a DBA verdict" || !strings.HasPrefix(wc.Why, "CR-01") {
		t.Fatalf("review case %+v", wc)
	}
	if len(rc.ActionCandidates) != 0 || len(wc.ActionCandidates) != 0 {
		t.Fatal("change review cases carry no action candidates")
	}
	if _, ok := caseByReview(got, safe.ID); ok {
		t.Fatal("a safe review must never be a case")
	}
	if rc.Evidence[0].Detail["link"] !=
		"#/reviews?id="+risky.ID+"&database=db1" {
		t.Fatalf("link %v", rc.Evidence[0].Detail["link"])
	}
}

func TestCasesDropAReviewOnceADBADecides(t *testing.T) {
	f := newCRCasesFixture(t)
	risky := submitCR(t, f.svc, "ALTER TABLE orders DROP COLUMN email; "+crfake.MarkRisky)
	review := submitCR(t, f.svc, "ALTER TABLE orders ALTER c TYPE text "+crfake.MarkReview)
	if len(getCases(t, f.mgr, "?database=db1")) != 2 {
		t.Fatal("both reviews must be cases before a verdict")
	}
	if _, err := f.svc.Decide(context.Background(), risky.ID, changereview.DBAVerdict{
		Verdict: changereview.DBAConcur, Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	got := getCases(t, f.mgr, "?database=db1")
	if len(got) != 1 || got[0].IdentityKey != "change_review:"+review.ID {
		t.Fatalf("after concur: %+v, want only the undecided review", got)
	}
	if _, err := f.svc.Decide(context.Background(), review.ID, changereview.DBAVerdict{
		Verdict: changereview.DBAOverride, Direction: changereview.DirectionTooStrict,
		Reason: "lock is brief here", Actor: "user:1"}); err != nil {
		t.Fatal(err)
	}
	if got := getCases(t, f.mgr, "?database=db1"); len(got) != 0 {
		t.Fatalf("after override: %+v, want no change review case", got)
	}
}

func TestCasesReadEachDatabasesReviewsFromItsOwnPool(t *testing.T) {
	f := newCRCasesFixture(t)
	r1 := submitCR(t, f.svc, "ALTER TABLE orders DROP COLUMN email; "+crfake.MarkRisky)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool2, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "api_cr_cases2"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool2.Close)
	if err := schema.Bootstrap(ctx, pool2); err != nil {
		t.Fatalf("bootstrap db2: %v", err)
	}
	svc2 := crfake.Start(t, pool2, "db2", &crfake.Analyzer{}, nil)
	r2 := submitCR(t, svc2, "ALTER TABLE items ALTER x TYPE text "+crfake.MarkReview)
	f.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "db2",
		Config: config.DatabaseConfig{Name: "db2"}, Pool: pool2,
		Status: &fleet.InstanceStatus{Connected: true}})

	all := getCases(t, f.mgr, "?database=all")
	c1, ok1 := caseByReview(all, r1.ID)
	c2, ok2 := caseByReview(all, r2.ID)
	if len(all) != 2 || !ok1 || !ok2 || c1.DatabaseName != "db1" || c2.DatabaseName != "db2" {
		t.Fatalf("fleet cases %+v, want db1's and db2's review under their own names", all)
	}
	only2 := getCases(t, f.mgr, "?database=db2")
	if len(only2) != 1 || only2[0].IdentityKey != "change_review:"+r2.ID {
		t.Fatalf("db2 cases %+v, want only db2's review", only2)
	}
}

func TestQueryChangeReviewCasesWithoutTheTable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "api_cr_cases_notable"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE sage.change_review`); err != nil {
		t.Fatal(err)
	}
	got, err := queryChangeReviewCases(ctx, namedPool{name: "old", pool: pool}, time.Now())
	if err != nil || len(got) != 0 {
		t.Fatalf("a schema without sage.change_review: %v %+v, want no cases", err, got)
	}
}

func TestQueryChangeReviewCasesReportsReadErrors(t *testing.T) {
	f := newCRCasesFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := queryChangeReviewCases(ctx, namedPool{name: "db1", pool: f.pool}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "change review cases of db1") {
		t.Fatalf("cancelled read: %v, want an error naming the change review read", err)
	}
}

func TestChangeReviewCasesQueryUsesTheDBAQueueIndex(t *testing.T) {
	f := newCRCasesFixture(t)
	ctx := context.Background()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET enable_seqscan`) }()
	var plan string
	if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+changeReviewCasesSQL,
		changeReviewCasesLimit).Scan(&plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "change_review_dba_queue") {
		t.Fatalf("plan %s, want an index scan of change_review_dba_queue", plan)
	}
	if !strings.HasPrefix(strings.TrimSpace(changeReviewCasesSQL),
		"/* pg_sage change_review_cases v1 */") {
		t.Fatal("the statement must carry its pg_sage tag")
	}
}

// Post-test audit: the read is bounded at changeReviewCasesLimit and keeps
// the most recently completed reviews (a PR closed without a verdict must
// not crowd new work out of the queue).
func TestQueryChangeReviewCasesKeepsTheNewestCompleted(t *testing.T) {
	f := newCRCasesFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO sage.change_review (database_name, source,
		actor, input_hash, state, stage, verdict, deterministic_verdict, parse_mode,
		model_status, landing_state, receipt_verdict, completed_at, findings)
		SELECT 'db1', 'mcp', 'a', md5(g::text) || md5(g::text), 'complete', 'done',
		'review', 'review', 'ast', 'no_model', 'awaiting', 'pending',
		now() - make_interval(hours => g),
		'[{"rule_id":"CR-01","severity":"warning","title":"t"}]'::jsonb
		FROM generate_series(1, $1::int + 1) g`, changeReviewCasesLimit); err != nil {
		t.Fatalf("seed reviews: %v", err)
	}
	got, err := queryChangeReviewCases(ctx, namedPool{name: "db1", pool: f.pool},
		time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != changeReviewCasesLimit {
		t.Fatalf("got %d cases, want the bound %d", len(got), changeReviewCasesLimit)
	}
	oldest := fmt.Sprintf("waiting %dh for a DBA verdict", changeReviewCasesLimit+1)
	for _, c := range got {
		if c.WhyNow == oldest {
			t.Fatalf("the oldest completed review %q must be the one left out", c.WhyNow)
		}
	}
}
