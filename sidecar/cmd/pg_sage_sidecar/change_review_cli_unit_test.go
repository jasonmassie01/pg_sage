package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// Post-test audit of 'pg_sage review': the gate table, the data-access
// classifier, diff parsing, endpoint building, token sources and the
// paths the end-to-end tests do not reach.

func TestReviewGateTable(t *testing.T) {
	done := func(verdict string) changereview.Review {
		return changereview.Review{State: "complete", Verdict: verdict,
			Summary: changereview.ReviewSummary{Critical: 1, Warning: 2}}
	}
	decided := func(verdict, dba, dir string) changereview.Review {
		r := done(verdict)
		r.DBAVerdict, r.DBADirection, r.DBAActor = dba, dir, "user:2"
		return r
	}
	cases := []struct {
		name   string
		r      changereview.Review
		failOn string
		state  string
		pass   bool
		desc   string
	}{
		{"running", changereview.Review{State: "running"}, "risky", "pending", false, "reviewing"},
		{"running never", changereview.Review{State: "running"}, "never", "pending", true, ""},
		{"failed", changereview.Review{State: "failed", FailureReason: "interrupted"}, "risky",
			"error", false, "interrupted"},
		{"safe", done("safe"), "risky", "success", true, "no blocking findings"},
		{"review below risky", done("review"), "risky", "success", true, "DBA review"},
		{"review at review", done("review"), "review", "failure", false, "1 critical, 2 warning"},
		{"risky", done("risky"), "risky", "failure", false, "a DBA verdict is needed"},
		{"risky never", done("risky"), "never", "success", true, "risky"},
		{"concur", decided("risky", "concur", ""), "risky", "success", true,
			"DBA concurred (user:2)"},
		{"with edits", decided("risky", "concur_with_edits", ""), "review", "success", true,
			"with edits"},
		{"too strict", decided("risky", "override", "too_strict"), "risky", "success", true,
			"too strict"},
		{"too lenient", decided("safe", "override", "too_lenient"), "risky", "failure", false,
			"too lenient"},
		{"too lenient never", decided("safe", "override", "too_lenient"), "never", "failure",
			true, "too lenient"},
	}
	for _, tc := range cases {
		g := gateOf(tc.r, tc.failOn)
		if g.state != tc.state || g.pass != tc.pass || !strings.Contains(g.description,
			tc.desc) || len([]rune(g.description)) > maxStatusDescription {
			t.Errorf("%s: %+v, want state %s pass %v desc ~%q", tc.name, g, tc.state, tc.pass,
				tc.desc)
		}
	}
}

func TestReviewDataAccessPath(t *testing.T) {
	yes := []string{"db/migrate/20261023_rename.rb", "migrations/V2__add.sql",
		"alembic/versions/3f2_add.py", "prisma/schema.prisma", "sql/report.SQL",
		"app/models/order.rb", "src/repositories/orders.ts", "internal/dao/user.go",
		"app/orders/migrations/0002_auto.py", "db/schema.rb"}
	no := []string{"README.md", "web/site.css", "docs/img/logo.png", "app/controllers/a.rb",
		"docs/schema/diagram.png", "Makefile", "models.md"}
	for _, p := range yes {
		if !reviewDataAccessPath(p) {
			t.Errorf("%s must count as database-touching", p)
		}
	}
	for _, p := range no {
		if reviewDataAccessPath(p) {
			t.Errorf("%s must not count as database-touching", p)
		}
	}
}

func TestDiffChangedPaths(t *testing.T) {
	diff := "diff --git a/old.sql b/new.sql\nrename from old.sql\n--- a/old.sql\n" +
		"+++ b/new.sql\n@@ -1 +1 @@\n-x\n+y\n" +
		"--- a/gone.sql\t2026-10-23 10:00:00\n+++ /dev/null\n@@ -1 +0,0 @@\n-z\n" +
		"--- /dev/null\n+++ b/added.sql\n@@ -0,0 +1 @@\n+w\n" +
		"--- a/new.sql\n+++ b/new.sql\n"
	got := diffChangedPaths(strings.ReplaceAll(diff, "\n", "\r\n"))
	want := []string{"old.sql", "new.sql", "gone.sql", "added.sql"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed paths %v, want %v", got, want)
	}
	if paths := diffChangedPaths(""); len(paths) != 0 {
		t.Fatalf("empty diff: %v", paths)
	}
}

func TestNewMCPClientEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sage.example.com":             "https://sage.example.com/api/v1/mcp",
		"https://sage.example.com:8080/":       "https://sage.example.com:8080/api/v1/mcp",
		"https://h/sage":                       "https://h/sage/api/v1/mcp",
		"https://h/api/v1/mcp":                 "https://h/api/v1/mcp",
		"http://localhost:8080?x=1#y":          "http://localhost:8080/api/v1/mcp",
		"http://127.0.0.1:9":                   "http://127.0.0.1:9/api/v1/mcp",
		"http://[::1]:9":                       "http://[::1]:9/api/v1/mcp",
		" https://sage.example.com/sage/api/ ": "https://sage.example.com/sage/api/api/v1/mcp",
	} {
		c, err := newMCPClient(raw, "t", nil)
		if err != nil || c.endpoint != want {
			t.Errorf("%q: %v %v, want %s", raw, c, err, want)
		}
	}
	for _, raw := range []string{"", "sage.example.com", "ftp://h", "http://10.0.0.5",
		"https://user:pw@h", "https://"} {
		if _, err := newMCPClient(raw, "t", nil); exitCode(err) != reviewExitUsage {
			t.Errorf("%q: %v, want a usage error", raw, err)
		}
	}
}

func TestReviewTokenSources(t *testing.T) {
	m := newCRMCP(t, verdictTools("safe"))
	e := newCREnv(t)
	delete(e.vars, "PG_SAGE_TOKEN_FILE")
	e.vars["PG_SAGE_TOKEN"] = crTestToken
	if code := e.run(t, submitArgs(t, m)...); code != 0 {
		t.Fatalf("PG_SAGE_TOKEN: exit %d (%s)", code, e.stderr)
	}
	e = newCREnv(t)
	empty := writeFile(t, t.TempDir(), "empty", "  \n")
	if code := e.run(t, submitArgs(t, m, "--token-file", empty)...); code != 2 ||
		!strings.Contains(e.stderr.String(), "is empty") {
		t.Fatalf("empty token file: exit %d %q", code, e.stderr)
	}
	failing := newCRMCP(t, func(string, map[string]any) (map[string]any, int) {
		return nil, -32001
	})
	failing.errMsg = "token " + crTestToken + " is not allowed here"
	e = newCREnv(t)
	if code := e.run(t, submitArgs(t, failing)...); code != 3 ||
		strings.Contains(e.stderr.String(), crTestToken) ||
		!strings.Contains(e.stderr.String(), "[redacted]") {
		t.Fatalf("a server message echoing the token: exit %d %q", code, e.stderr)
	}
}

func TestReviewSubmitRefusesAnOversizedRequestLocally(t *testing.T) {
	m := newCRMCP(t, verdictTools("safe"))
	e := newCREnv(t)
	big := "--- a/db/x.sql\n+++ b/db/x.sql\n@@ -0,0 +1 @@\n+" + strings.Repeat("y", 1<<20)
	diff := writeFile(t, t.TempDir(), "big.diff", big)
	if code := e.run(t, "submit", "--url", m.srv.URL, "--database", "d", "--diff-file",
		diff); code != 2 || !strings.Contains(e.stderr.String(), "byte MCP request limit") {
		t.Fatalf("oversized: exit %d %q", code, e.stderr)
	}
	if len(m.requests()) != 0 {
		t.Fatal("an oversized request must not reach pg_sage")
	}
}

func TestReviewDeployedTwiceIsNotAFailure(t *testing.T) {
	m := newCRMCP(t, func(string, map[string]any) (map[string]any, int) { return nil, -32009 })
	e := newCREnv(t)
	if code := e.run(t, "deployed", "--url", m.srv.URL, "--database", "d", "--id",
		crReviewID, "--deployed-at", "2026-10-23T11:00:00+02:00"); code != 0 ||
		!strings.Contains(e.stdout.String(), "already recorded") {
		t.Fatalf("deployed twice: exit %d %q", code, e.stdout)
	}
	if got := m.toolCalls()[0].Args["deployed_at"]; got != "2026-10-23T09:00:00Z" {
		t.Fatalf("deployed_at %v, want it in UTC", got)
	}
	e = newCREnv(t)
	if code := e.run(t, "deployed", "--url", m.srv.URL, "--database", "d", "--id", "x",
		"--pr-url", "https://github.com/o/r/pull/7"); code != 2 {
		t.Fatalf("--id and --pr-url: exit %d, want 2", code)
	}
}

func TestGitHubModeWithoutAPullRequestSetsTheCommitStatusOnly(t *testing.T) {
	g := newFakeGitHub(t)
	e := githubEnv(t, g, `{"inputs":{}}`)
	e.vars["GITHUB_SHA"] = "fedcba9"
	sql := writeFile(t, t.TempDir(), "a.sql", "SELECT 1")
	m := newCRMCP(t, verdictTools("safe"))
	if code := e.run(t, "submit", "--github", "--url", m.srv.URL, "--database", "d",
		"--sql-file", sql); code != 0 {
		t.Fatalf("workflow_dispatch: exit %d (%s)", code, e.stderr)
	}
	comments, statuses, _ := g.snapshot()
	// crReview names head_sha abcdef1: the reviewed commit wins over GITHUB_SHA.
	if len(comments) != 0 || len(statuses) != 1 || statuses[0].SHA != "abcdef1" {
		t.Fatalf("comments %+v statuses %+v", comments, statuses)
	}
}

func TestGitHubStatusForThePullRequestOfTheEvent(t *testing.T) {
	g := newFakeGitHub(t)
	m := newCRMCP(t, func(tool string, _ map[string]any) (map[string]any, int) {
		r := crReview(crReviewID, "complete", "review")
		if tool == "list_change_reviews" {
			return map[string]any{"reviews": []any{r}}, 0
		}
		return r, 0
	})
	e := githubEnv(t, g, crPREvent)
	if code := e.run(t, "status", "--github", "--url", m.srv.URL, "--database", "d",
		"--fail-on", "review"); code != 1 {
		t.Fatalf("status of a review verdict with --fail-on review: exit %d (%s)", code,
			e.stderr)
	}
	comments, statuses, _ := g.snapshot()
	if len(comments) != 1 || len(statuses) != 1 || statuses[0].State != "failure" {
		t.Fatalf("comments %d statuses %+v", len(comments), statuses)
	}
	if fmt.Sprint(m.toolCalls()[0].Args["pr_url"]) != "https://github.com/o/r/pull/7" {
		t.Fatalf("status must look the PR up: %+v", m.toolCalls()[0])
	}
}

func TestGitHubStatusJobSkipsAReviewOfAnOlderCommit(t *testing.T) {
	g := newFakeGitHub(t)
	m := newCRMCP(t, func(tool string, _ map[string]any) (map[string]any, int) {
		r := crReview(crReviewID, "complete", "safe")
		r["head_sha"] = "0000001" // the open pull request's head is abcdef1
		if tool == "list_change_reviews" {
			return map[string]any{"reviews": []any{r}}, 0
		}
		return r, 0
	})
	e := githubEnv(t, g, `{"schedule":"*/15 * * * *"}`)
	if code := e.run(t, "status", "--github", "--url", m.srv.URL, "--database", "d"); code != 0 {
		t.Fatalf("status job: exit %d (%s)", code, e.stderr)
	}
	if _, _, writes := g.snapshot(); len(writes) != 0 {
		t.Fatalf("a review of an older commit must not set the new head's status: %v", writes)
	}
}
