//go:build cgo

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/testsupport/crfake"
	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

// The production composition end to end: 'pg_sage review submit' over
// real MCP Streamable HTTP to the service buildChangeReview assembles for
// a runtime (impact engine, reasoner without a model, diffscan), on the
// rv fixture's workload. The surfaces' own tests stand the fake analyzer
// in for the engine; this one proves the real verdicts and citations
// reach the CLI. cgo: the engine parses with libpg_query.

// newRealCRStack serves MCP over HTTP for the rv fixture's database with
// the production Change Review service behind it.
func newRealCRStack(t *testing.T) (*httptest.Server, *rvfixture.Fixture) {
	t.Helper()
	f := rvfixture.New(t)
	cfg := config.DefaultConfig()
	cfg.LLM.Enabled = false
	svc, err := buildChangeReview(changeReviewParts{Database: f.Name, Pool: f.Pool,
		Config: cfg, Logf: crfake.Logf(t)})
	if err != nil {
		t.Fatalf("build the production change review: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := svc.Start(ctx); err != nil {
		t.Fatalf("start change review: %v", err)
	}
	reg := changereview.NewRegistry()
	reg.Set(f.Name, svc)
	server := mcp.NewServer(&crE2EBackend{ChangeReviewBackend: mcp.NewChangeReviewBackend(reg)})
	rt, err := mcp.NewRuntime(config.MCPConfig{Enabled: true, Transport: "http"}, server,
		nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	inner := rt.HTTPHandler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+crTestToken {
			http.Error(w, `{"code":"mcp_token_required"}`, http.StatusUnauthorized)
			return
		}
		p := mcp.Principal{Actor: "token:ci", Kind: mcp.KindAgent, TokenID: "ci",
			Scopes: []mcp.Scope{mcp.ScopeRead}, Databases: []string{f.Name}}
		inner.ServeHTTP(w, r.WithContext(mcp.WithPrincipal(r.Context(), p)))
	}))
	t.Cleanup(srv.Close)
	return srv, f
}

// submitReal runs 'pg_sage review submit --format json --wait --fail-on
// risky' for one SQL file and returns the exit code and the review.
func submitReal(t *testing.T, srv *httptest.Server, db, sql string) (int,
	changereview.Review) {
	t.Helper()
	e := newCREnv(t)
	file := writeFile(t, t.TempDir(), "migration.sql", sql)
	code := e.run(t, "submit", "--url", srv.URL, "--database", db, "--sql-file", file,
		"--format", "json", "--fail-on", "risky", "--wait")
	var r changereview.Review
	if err := json.Unmarshal(e.stdout.Bytes(), &r); err != nil {
		t.Fatalf("submit %q: exit %d, output not a review: %v (%s / %s)", sql, code, err,
			e.stdout, e.stderr)
	}
	return code, r
}

func findingsOf(r changereview.Review, rule string) []changereview.Finding {
	var out []changereview.Finding
	for _, f := range r.Findings {
		if f.RuleID == rule {
			out = append(out, f)
		}
	}
	return out
}

func TestRealStackSafeMigrationPasses(t *testing.T) {
	srv, f := newRealCRStack(t)
	code, r := submitReal(t, srv, f.Name, "ALTER TABLE rv.rv_orders ADD COLUMN note text;")
	if code != 0 || r.State != "complete" || r.Verdict != "safe" ||
		r.DeterministicVerdict != "safe" || r.ParseMode != "ast" || r.ModelStatus != "no_model" {
		t.Fatalf("safe migration: exit %d, %s %s/%s parse %s model %s", code, r.State,
			r.Verdict, r.DeterministicVerdict, r.ParseMode, r.ModelStatus)
	}
	for _, f := range r.Findings {
		if f.Severity != changereview.SeverityInfo {
			t.Errorf("a safe review carries a %s finding %s: %s", f.Severity, f.RuleID, f.Title)
		}
	}
	if len(r.Expectations.PostConditions) != 1 ||
		!strings.HasPrefix(r.Markdown, "<!-- pg_sage-review db="+f.Name+" id="+r.ID) {
		t.Fatalf("post-conditions %+v, markdown head %.80q", r.Expectations.PostConditions,
			r.Markdown)
	}
}

func TestRealStackRenameOfAHotColumnIsRiskyAndCitesTheQuery(t *testing.T) {
	srv, f := newRealCRStack(t)
	code, r := submitReal(t, srv, f.Name,
		"ALTER TABLE rv.rv_orders RENAME COLUMN email TO email_address;")
	if code != 1 || r.Verdict != "risky" {
		t.Fatalf("rename: exit %d verdict %s, want 1 and risky", code, r.Verdict)
	}
	cr03 := findingsOf(r, "CR-03")
	if len(cr03) != 1 || cr03[0].Severity != changereview.SeverityCritical {
		t.Fatalf("CR-03 findings %+v, want one critical", cr03)
	}
	q1 := "queryid:" + strconv.FormatInt(f.Q1.ID, 10)
	if !slices.Contains(cr03[0].EvidenceIDs, q1) {
		t.Fatalf("CR-03 cites %v, want Q1's %s", cr03[0].EvidenceIDs, q1)
	}
	if slices.Contains(cr03[0].EvidenceIDs, "queryid:"+strconv.FormatInt(f.Q2.ID, 10)) {
		t.Fatalf("CR-03 cites Q2, which never reads email: %v", cr03[0].EvidenceIDs)
	}
	if !strings.Contains(r.Markdown, q1) || !strings.Contains(r.Markdown, "CR-03") {
		t.Fatal("the Markdown comment does not carry the CR-03 citation of Q1")
	}
}

func TestRealStackSetNotNullOverNullsWillFail(t *testing.T) {
	srv, f := newRealCRStack(t)
	f.Exec("UPDATE rv.rv_orders SET customer_id = NULL WHERE id % 100 < 3")
	f.Exec("ANALYZE rv.rv_orders")
	code, r := submitReal(t, srv, f.Name,
		"ALTER TABLE rv.rv_orders ALTER COLUMN customer_id SET NOT NULL;")
	if code != 1 || r.Verdict != "risky" {
		t.Fatalf("SET NOT NULL over NULLs: exit %d verdict %s, want 1 and risky", code,
			r.Verdict)
	}
	cr05 := findingsOf(r, "CR-05")
	if len(cr05) == 0 || cr05[0].Severity != changereview.SeverityCritical ||
		len(cr05[0].EvidenceIDs) == 0 {
		t.Fatalf("CR-05 findings %+v, want a cited critical", cr05)
	}
}
