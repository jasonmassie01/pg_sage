package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 'pg_sage review submit|status|deployed' against a scripted MCP server
// (CHECK-SRF-03): exit 0 pass, 1 verdict at or above --fail-on, 2 usage,
// 3 transport. The token from --token-file or PG_SAGE_TOKEN_FILE never
// appears in stdout or stderr.

// verdictTools answers review_change with a complete review of verdict.
func verdictTools(verdict string) func(string, map[string]any) (map[string]any, int) {
	return func(tool string, _ map[string]any) (map[string]any, int) {
		r := crReview(crReviewID, "complete", verdict)
		if tool == "review_change" {
			r["created"] = true
		}
		return r, 0
	}
}

func submitArgs(t *testing.T, m *crMCP, extra ...string) []string {
	sql := writeFile(t, t.TempDir(), "a.sql", "ALTER TABLE orders ADD COLUMN note text;")
	args := []string{"submit", "--url", m.srv.URL, "--database", "d", "--sql-file", sql}
	return append(args, extra...)
}

func TestReviewSubmitExitCodesFollowTheVerdictAndFailOn(t *testing.T) {
	cases := []struct {
		verdict, failOn string
		want            int
	}{
		{"safe", "risky", 0}, {"review", "risky", 0}, {"risky", "risky", 1},
		{"review", "review", 1}, {"safe", "review", 0}, {"risky", "never", 0},
	}
	for _, tc := range cases {
		m := newCRMCP(t, verdictTools(tc.verdict))
		e := newCREnv(t)
		diff := writeFile(t, t.TempDir(), "pr.diff", crSQLDiff)
		got := e.run(t, submitArgs(t, m, "--diff-file", diff, "--fail-on", tc.failOn,
			"--format", "json")...)
		if got != tc.want {
			t.Errorf("%s with --fail-on %s: exit %d, want %d (stderr %s)", tc.verdict,
				tc.failOn, got, tc.want, e.stderr)
		}
		var out map[string]any
		if err := json.Unmarshal(e.stdout.Bytes(), &out); err != nil || out["verdict"] !=
			tc.verdict {
			t.Errorf("--format json: %v %s", err, e.stdout)
		}
	}
}

func TestReviewSubmitSendsTheRequestOverMCP(t *testing.T) {
	m := newCRMCP(t, verdictTools("safe"))
	e := newCREnv(t)
	diff := writeFile(t, t.TempDir(), "pr.diff", crSQLDiff)
	code := e.run(t, submitArgs(t, m, "--diff-file", diff, "--pr-url",
		"https://github.com/o/r/pull/7", "--head-sha", "abcdef1", "--migration-tool", "flyway",
		"--transactional=false")...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e.stderr)
	}
	calls := m.toolCalls()
	if len(calls) != 1 || calls[0].Tool != "review_change" {
		t.Fatalf("calls %+v, want one review_change", calls)
	}
	a := calls[0].Args
	files, _ := a["sql_files"].([]any)
	pr, _ := a["pr"].(map[string]any)
	if a["database"] != "d" || len(files) != 1 || a["diff"] != crSQLDiff ||
		a["migration_tool"] != "flyway" || a["transactional"] != false ||
		pr["url"] != "https://github.com/o/r/pull/7" || pr["head_sha"] != "abcdef1" ||
		a["wait_seconds"] != float64(50) {
		t.Fatalf("review_change arguments %v", a)
	}
	first, _ := files[0].(map[string]any)
	if first["content"] != "ALTER TABLE orders ADD COLUMN note text;" ||
		!strings.HasSuffix(first["path"].(string), "a.sql") {
		t.Fatalf("sql file %v", first)
	}
	if !strings.HasPrefix(e.stdout.String(), "<!-- pg_sage-review db=d id="+crReviewID) {
		t.Fatalf("markdown output %q must start with the marker", e.stdout)
	}
}

func TestReviewSubmitUsageErrorsExit2WithoutACall(t *testing.T) {
	m := newCRMCP(t, verdictTools("safe"))
	sql := writeFile(t, t.TempDir(), "a.sql", "SELECT 1")
	sub := func(extra ...string) []string {
		return append([]string{"submit", "--url", m.srv.URL, "--database", "d"}, extra...)
	}
	cases := map[string][]string{
		"no subcommand":       {},
		"unknown subcommand":  {"approve"},
		"no database":         {"submit", "--url", m.srv.URL, "--sql-file", sql},
		"no url":              {"submit", "--database", "d", "--sql-file", sql},
		"no input":            sub(),
		"bad fail-on":         sub("--sql-file", sql, "--fail-on", "warning"),
		"bad format":          sub("--sql-file", sql, "--format", "xml"),
		"missing sql file":    sub("--sql-file", sql+".nope"),
		"two diff sources":    sub("--diff-file", sql, "--git-diff", "a...b"),
		"bad git range":       sub("--git-diff", "--output=/tmp/x...b"),
		"changed-only no dir": sub("--changed-only", "--diff-file", sql),
		"bad transactional":   sub("--sql-file", sql, "--transactional=maybe"),
		"unknown flag":        sub("--sql-file", sql, "--token", "x"),
		"url credentials": {"submit", "--url", "https://u:p@sage.example.com", "--database",
			"d", "--sql-file", sql},
		"plain http remote": {"submit", "--url", "http://sage.example.com", "--database", "d",
			"--sql-file", sql},
		"status without id": {"status", "--url", m.srv.URL, "--database", "d"},
		"deployed bad time": {"deployed", "--url", m.srv.URL, "--database", "d", "--id",
			crReviewID, "--deployed-at", "noon"},
	}
	for name, args := range cases {
		e := newCREnv(t)
		if got := e.run(t, args...); got != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr %s)", name, got, e.stderr)
		}
		if !strings.Contains(e.stderr.String(), "usage") &&
			!strings.Contains(e.stderr.String(), "pg_sage review") {
			t.Errorf("%s: stderr %q should explain the usage", name, e.stderr)
		}
	}
	if calls := m.toolCalls(); len(calls) != 0 {
		t.Fatalf("usage errors made %d MCP calls", len(calls))
	}
	e := newCREnv(t)
	delete(e.vars, "PG_SAGE_TOKEN_FILE")
	if got := e.run(t, "submit", "--url", m.srv.URL, "--database", "d", "--sql-file",
		sql); got != 2 || !strings.Contains(e.stderr.String(), "PG_SAGE_TOKEN_FILE") {
		t.Fatalf("no token: exit %d %q, want 2 naming PG_SAGE_TOKEN_FILE", got, e.stderr)
	}
}

func TestReviewSubmitTransportFailuresExit3AndNeverLeakTheToken(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError,
		http.StatusRequestEntityTooLarge} {
		m := newCRMCP(t, verdictTools("safe"))
		m.status = status
		e := newCREnv(t)
		if got := e.run(t, submitArgs(t, m)...); got != 3 {
			t.Errorf("HTTP %d: exit %d, want 3", status, got)
		}
		out := e.stdout.String() + e.stderr.String()
		if strings.Contains(out, crTestToken) || !strings.Contains(out, "HTTP") {
			t.Errorf("HTTP %d output %q: must name the status and never the token", status,
				out)
		}
		if !strings.Contains(m.requests(), "Bearer "+crTestToken) {
			t.Errorf("HTTP %d: the token was not sent as a bearer token", status)
		}
	}
	e := newCREnv(t)
	if got := e.run(t, "submit", "--url", "http://127.0.0.1:1", "--database", "d",
		"--sql-file", writeFile(t, t.TempDir(), "a.sql", "SELECT 1")); got != 3 {
		t.Fatalf("connection refused: exit %d, want 3", got)
	}
}

func TestReviewToolErrorsMapToExitCodes(t *testing.T) {
	for code, want := range map[int]int{-32602: 2, -32004: 2, -32006: 2, -32007: 2,
		-32001: 3, -32003: 3, -32010: 3, -32603: 3} {
		m := newCRMCP(t, func(string, map[string]any) (map[string]any, int) { return nil, code })
		e := newCREnv(t)
		if got := e.run(t, submitArgs(t, m)...); got != want {
			t.Errorf("tool error %d: exit %d, want %d", code, got, want)
		}
		if !strings.Contains(e.stderr.String(), "refused by the fake") {
			t.Errorf("tool error %d: stderr %q must carry the server's message", code, e.stderr)
		}
	}
}

func TestReviewSubmitWaitPollsEveryFiveSeconds(t *testing.T) {
	gets := 0
	m := newCRMCP(t, func(tool string, _ map[string]any) (map[string]any, int) {
		if tool == "review_change" {
			return crReview(crReviewID, "running", ""), 0
		}
		gets++
		if gets < 3 {
			return crReview(crReviewID, "running", ""), 0
		}
		return crReview(crReviewID, "complete", "risky"), 0
	})
	e := newCREnv(t)
	if got := e.run(t, submitArgs(t, m, "--wait", "--fail-on", "risky")...); got != 1 {
		t.Fatalf("exit %d, want 1 for the risky verdict reached by polling (%s)", got, e.stderr)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}
	if gets != 3 || !reflect.DeepEqual(e.sleeps, want) {
		t.Fatalf("%d gets, sleeps %v, want 3 gets 5 s apart", gets, e.sleeps)
	}
	for _, c := range m.toolCalls()[1:] {
		if c.Tool != "get_change_review" || c.Args["id"] != crReviewID {
			t.Fatalf("poll call %+v", c)
		}
	}
}

func TestReviewSubmitWithoutWaitTreatsARunningReviewAsNotPassing(t *testing.T) {
	m := newCRMCP(t, func(string, map[string]any) (map[string]any, int) {
		return crReview(crReviewID, "running", ""), 0
	})
	e := newCREnv(t)
	if got := e.run(t, submitArgs(t, m, "--fail-on", "risky")...); got != 1 {
		t.Fatalf("running review with --fail-on risky: exit %d, want 1 (fail closed)", got)
	}
	if !strings.Contains(e.stderr.String(), "still running") || len(e.sleeps) != 0 {
		t.Fatalf("stderr %q sleeps %v", e.stderr, e.sleeps)
	}
	e = newCREnv(t)
	if got := e.run(t, submitArgs(t, m, "--fail-on", "never")...); got != 0 {
		t.Fatalf("running review with --fail-on never: exit %d, want 0", got)
	}
	e = newCREnv(t)
	if got := e.run(t, submitArgs(t, m, "--wait=12s")...); got != 1 || len(e.sleeps) != 3 {
		t.Fatalf("--wait=12s: exit %d after %d polls, want 1 after 3", got, len(e.sleeps))
	}
}

func TestReviewSubmitSkipsWhenNothingTouchesTheDatabase(t *testing.T) {
	m := newCRMCP(t, verdictTools("risky"))
	e := newCREnv(t)
	diff := writeFile(t, t.TempDir(), "pr.diff", crDocsDiff)
	got := e.run(t, "submit", "--url", m.srv.URL, "--database", "d", "--diff-file", diff,
		"--fail-on", "risky")
	if got != 0 || !strings.Contains(e.stdout.String(), "no database-touching changes") {
		t.Fatalf("docs-only diff: exit %d stdout %q", got, e.stdout)
	}
	if len(m.requests()) != 0 {
		t.Fatalf("the skip path must not call MCP: %s", m.requests())
	}
}

func TestReviewSubmitSQLDirChangedOnly(t *testing.T) {
	m := newCRMCP(t, verdictTools("safe"))
	dir := t.TempDir()
	writeFile(t, dir, "db/migrate/001_init.sql", "CREATE TABLE orders (id bigint);")
	writeFile(t, dir, "db/migrate/002_add_note.sql", "ALTER TABLE orders ADD COLUMN note text;")
	writeFile(t, dir, "db/migrate/notes.txt", "not sql")
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	e := newCREnv(t)
	if got := e.run(t, "submit", "--url", m.srv.URL, "--database", "d", "--sql-dir",
		"db/migrate", "--changed-only", "--git-diff", "origin/main...HEAD"); got != 0 {
		t.Fatalf("exit %d: %s", got, e.stderr)
	}
	if !reflect.DeepEqual(e.gitArgs, [][]string{{"diff", "--no-color", "--no-ext-diff",
		"origin/main...HEAD"}}) {
		t.Fatalf("git invoked as %v", e.gitArgs)
	}
	files, _ := m.toolCalls()[0].Args["sql_files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != "db/migrate/002_add_note.sql" {
		t.Fatalf("--changed-only sent %v, want only the changed migration", files)
	}
	e = newCREnv(t)
	if got := e.run(t, "submit", "--url", m.srv.URL, "--database", "d", "--sql-dir",
		"db/migrate"); got != 0 {
		t.Fatalf("whole dir: exit %d: %s", got, e.stderr)
	}
	files, _ = m.toolCalls()[1].Args["sql_files"].([]any)
	if len(files) != 2 || files[0].(map[string]any)["path"] != "db/migrate/001_init.sql" {
		t.Fatalf("--sql-dir sent %v, want both .sql files in order", files)
	}
}

func TestReviewStatusAndDeployed(t *testing.T) {
	m := newCRMCP(t, func(tool string, args map[string]any) (map[string]any, int) {
		switch tool {
		case "list_change_reviews":
			return map[string]any{"reviews": []any{crReview(crReviewID, "complete", "safe")}}, 0
		case "report_change_deployed":
			r := crReview(crReviewID, "complete", "safe")
			r["landing_state"], r["landing_source"] = "landed", "reported"
			return r, 0
		}
		return crReview(crReviewID, "complete", "review"), 0
	})
	e := newCREnv(t)
	if got := e.run(t, "status", "--url", m.srv.URL, "--database", "d", "--id", crReviewID,
		"--fail-on", "review"); got != 1 {
		t.Fatalf("status of a review verdict with --fail-on review: exit %d", got)
	}
	e = newCREnv(t)
	if got := e.run(t, "status", "--url", m.srv.URL, "--database", "d", "--pr-url",
		"https://github.com/o/r/pull/7"); got != 0 {
		t.Fatalf("status by PR: exit %d %s", got, e.stderr)
	}
	e = newCREnv(t)
	if got := e.run(t, "deployed", "--url", m.srv.URL, "--database", "d", "--pr-url",
		"https://github.com/o/r/pull/7", "--commit", "abcdef1"); got != 0 ||
		!strings.Contains(e.stdout.String(), "landed") {
		t.Fatalf("deployed: exit %d %q", got, e.stdout)
	}
	calls := m.toolCalls()
	last := calls[len(calls)-1]
	if last.Tool != "report_change_deployed" || last.Args["deployed_at"] !=
		"2026-10-23T12:00:00Z" || last.Args["commit"] != "abcdef1" {
		t.Fatalf("deploy call %+v, want now and the commit", last)
	}
	if calls[1].Tool != "list_change_reviews" || calls[1].Args["limit"] != float64(1) {
		t.Fatalf("status by PR call %+v", calls[1])
	}
}

func TestReviewOutWritesTheFile(t *testing.T) {
	m := newCRMCP(t, verdictTools("safe"))
	e := newCREnv(t)
	out := filepath.Join(t.TempDir(), "review.md")
	if got := e.run(t, submitArgs(t, m, "--out", out)...); got != 0 {
		t.Fatalf("exit %d", got)
	}
	raw, err := os.ReadFile(out)
	if err != nil || !strings.HasPrefix(string(raw), "<!-- pg_sage-review db=d") ||
		e.stdout.Len() != 0 {
		t.Fatalf("--out: %v %q, stdout %q", err, raw, e.stdout)
	}
}
