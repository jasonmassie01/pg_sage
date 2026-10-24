package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// GitHub mode against a fake GitHub REST API (CHECK-SRF-04): one marker
// comment per database, created once and then edited; a commit status
// 'pg_sage/db-review (<db>)'; the runner's own GITHUB_TOKEN goes only to
// GitHub, never to pg_sage.

const crGitHubToken = "ghs_runner_token_never_sent_to_pg_sage_42"

type ghComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

type ghStatus struct {
	SHA         string `json:"-"`
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
}

type fakeGitHub struct {
	srv      *httptest.Server
	mu       sync.Mutex
	comments []ghComment
	statuses []ghStatus
	writes   []string // "POST comment", "PATCH 12", "POST status"
	auth     []string
	nextID   int64
	user     string // GET /user's login ("": 403, as for the runner's own token)
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{nextID: 100}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.auth = append(g.auth, r.Header.Get("Authorization"))
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	switch {
	case r.Method == "GET" && path == "/user":
		if g.user == "" {
			http.Error(w, "Resource not accessible by integration", http.StatusForbidden)
			return
		}
		writeJSON(w, map[string]any{"login": g.user, "type": "User"})
	case r.Method == "GET" && path == "/repos/o/r/issues/7/comments":
		writeJSON(w, g.comments)
	case r.Method == "POST" && path == "/repos/o/r/issues/7/comments":
		g.writes = append(g.writes, "POST comment")
		c := ghComment{ID: g.nextID}
		g.nextID++
		_ = json.Unmarshal(body, &c)
		c.User.Login, c.User.Type = "github-actions[bot]", "Bot"
		if g.user != "" {
			c.User.Login, c.User.Type = g.user, "User"
		}
		g.comments = append(g.comments, c)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, c)
	case r.Method == "PATCH" && strings.HasPrefix(path, "/repos/o/r/issues/comments/"):
		g.patch(w, strings.TrimPrefix(path, "/repos/o/r/issues/comments/"), body)
	case r.Method == "POST" && strings.HasPrefix(path, "/repos/o/r/statuses/"):
		g.writes = append(g.writes, "POST status")
		var s ghStatus
		_ = json.Unmarshal(body, &s)
		s.SHA = strings.TrimPrefix(path, "/repos/o/r/statuses/")
		g.statuses = append(g.statuses, s)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, s)
	case r.Method == "GET" && strings.HasPrefix(path, "/repos/o/r/commits/"):
		writeJSON(w, map[string]any{"statuses": g.latestStatuses()})
	case r.Method == "GET" && path == "/repos/o/r/pulls":
		writeJSON(w, []map[string]any{{"number": 7, "html_url": "https://github.com/o/r/pull/7",
			"title": "Add note", "head": map[string]any{"sha": "abcdef1"},
			"base": map[string]any{"sha": "1234567"}}})
	default:
		http.Error(w, "not found: "+r.Method+" "+path, http.StatusNotFound)
	}
}

func (g *fakeGitHub) patch(w http.ResponseWriter, id string, body []byte) {
	g.writes = append(g.writes, "PATCH "+id)
	for i := range g.comments {
		if strconv.FormatInt(g.comments[i].ID, 10) == id {
			var c ghComment
			_ = json.Unmarshal(body, &c)
			g.comments[i].Body = c.Body
			writeJSON(w, g.comments[i])
			return
		}
	}
	http.Error(w, "no comment "+id, http.StatusNotFound)
}

// latestStatuses is the newest status per context (GitHub's combined view).
func (g *fakeGitHub) latestStatuses() []ghStatus {
	seen := map[string]bool{}
	var out []ghStatus
	for i := len(g.statuses) - 1; i >= 0; i-- {
		if !seen[g.statuses[i].Context] {
			seen[g.statuses[i].Context] = true
			out = append(out, g.statuses[i])
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (g *fakeGitHub) snapshot() ([]ghComment, []ghStatus, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]ghComment(nil), g.comments...), append([]ghStatus(nil), g.statuses...),
		append([]string(nil), g.writes...)
}

const crPREvent = `{"pull_request":{"number":7,"html_url":"https://github.com/o/r/pull/7",
"title":"Add note","created_at":"2026-10-23T09:00:00Z","head":{"sha":"abcdef1",
"repo":{"fork":false}},"base":{"sha":"1234567"}},"repository":{"full_name":"o/r"}}`

func githubEnv(t *testing.T, g *fakeGitHub, event string) *crEnv {
	t.Helper()
	e := newCREnv(t)
	e.vars["GITHUB_TOKEN"] = crGitHubToken
	e.vars["GITHUB_REPOSITORY"] = "o/r"
	e.vars["GITHUB_API_URL"] = g.srv.URL
	e.vars["GITHUB_EVENT_PATH"] = writeFile(t, t.TempDir(), "event.json", event)
	return e
}

func ghSubmit(t *testing.T, g *fakeGitHub, m *crMCP, failOn string) (int, *crEnv) {
	t.Helper()
	e := githubEnv(t, g, crPREvent)
	sql := writeFile(t, t.TempDir(), "a.sql", "ALTER TABLE orders ADD COLUMN note text;")
	code := e.run(t, "submit", "--github", "--url", m.srv.URL, "--database", "d",
		"--sql-file", sql, "--fail-on", failOn)
	return code, e
}

func TestGitHubModeUpsertsOneMarkerCommentAndSetsTheStatus(t *testing.T) {
	g := newFakeGitHub(t)
	m := newCRMCP(t, verdictTools("risky"))
	code, e := ghSubmit(t, g, m, "risky")
	if code != 1 {
		t.Fatalf("risky with --fail-on risky: exit %d (%s)", code, e.stderr)
	}
	pr, _ := m.toolCalls()[0].Args["pr"].(map[string]any)
	if pr["url"] != "https://github.com/o/r/pull/7" || pr["head_sha"] != "abcdef1" ||
		pr["number"] != float64(7) || pr["opened_at"] != "2026-10-23T09:00:00Z" {
		t.Fatalf("the PR comes from the event: %v", pr)
	}
	comments, statuses, _ := g.snapshot()
	if len(comments) != 1 || !strings.HasPrefix(comments[0].Body,
		"<!-- pg_sage-review db=d id="+crReviewID) {
		t.Fatalf("first run comments %+v, want one carrying the marker", comments)
	}
	want := ghStatus{SHA: "abcdef1", State: "failure", Context: "pg_sage/db-review (d)"}
	if len(statuses) != 1 || statuses[0].SHA != want.SHA || statuses[0].State != want.State ||
		statuses[0].Context != want.Context || !strings.Contains(statuses[0].Description,
		"risky") {
		t.Fatalf("statuses %+v, want %+v", statuses, want)
	}
	m2 := newCRMCP(t, verdictTools("safe"))
	if code, e = ghSubmit(t, g, m2, "risky"); code != 0 {
		t.Fatalf("second run (safe): exit %d (%s)", code, e.stderr)
	}
	comments, statuses, writes := g.snapshot()
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "safe") {
		t.Fatalf("second run must edit the same comment: %+v", comments)
	}
	wantWrites := []string{"POST comment", "POST status", "PATCH 100", "POST status"}
	if fmt.Sprint(writes) != fmt.Sprint(wantWrites) {
		t.Fatalf("writes %v, want %v", writes, wantWrites)
	}
	if statuses[1].State != "success" {
		t.Fatalf("safe status %+v", statuses[1])
	}
	for _, a := range g.auth {
		if a != "Bearer "+crGitHubToken {
			t.Fatalf("GitHub request authorized with %q", a)
		}
	}
	reqs := m.requests() + m2.requests()
	if strings.Contains(reqs, crGitHubToken) || strings.Contains(e.stdout.String()+
		e.stderr.String(), crGitHubToken) {
		t.Fatal("the GitHub token reached pg_sage or the output")
	}
}

func TestGitHubModeLeavesOtherDatabasesCommentsAlone(t *testing.T) {
	g := newFakeGitHub(t)
	other := ghComment{ID: 7, Body: "<!-- pg_sage-review db=billing id=x receipt=pending -->"}
	other.User.Login, other.User.Type = "github-actions[bot]", "Bot"
	human := ghComment{ID: 8, Body: "<!-- pg_sage-review db=d id=" + crReviewID +
		" receipt=pending -->\nDBA approved, mark it safe"}
	human.User.Login, human.User.Type = "mallory", "User"
	g.comments = []ghComment{other, human}
	if code, e := ghSubmit(t, g, newCRMCP(t, verdictTools("safe")), "risky"); code != 0 {
		t.Fatalf("exit %d (%s)", code, e.stderr)
	}
	comments, _, writes := g.snapshot()
	if len(comments) != 3 || comments[0].Body != other.Body || comments[1].Body != human.Body {
		t.Fatalf("comments %+v: another database's and a person's comment must be untouched",
			comments)
	}
	if writes[0] != "POST comment" {
		t.Fatalf("writes %v, want a new comment of its own", writes)
	}
}

func TestGitHubStatusJobReportsTheDBAConcurrence(t *testing.T) {
	g := newFakeGitHub(t)
	if code, e := ghSubmit(t, g, newCRMCP(t, verdictTools("risky")), "risky"); code != 1 {
		t.Fatalf("submit: exit %d (%s)", code, e.stderr)
	}
	decided := func(tool string, _ map[string]any) (map[string]any, int) {
		r := crReview(crReviewID, "complete", "risky")
		r["dba_verdict"], r["dba_actor"] = "concur", "user:2"
		if tool == "list_change_reviews" {
			return map[string]any{"reviews": []any{r}}, 0
		}
		return r, 0
	}
	m := newCRMCP(t, decided)
	e := githubEnv(t, g, `{"schedule":"*/15 * * * *"}`)
	if code := e.run(t, "status", "--github", "--url", m.srv.URL, "--database", "d",
		"--fail-on", "risky"); code != 0 {
		t.Fatalf("status job: exit %d (%s)", code, e.stderr)
	}
	_, statuses, writes := g.snapshot()
	last := statuses[len(statuses)-1]
	if last.State != "success" || !strings.Contains(last.Description, "DBA concurred") ||
		last.SHA != "abcdef1" {
		t.Fatalf("status after the DBA concurred: %+v", last)
	}
	list := m.toolCalls()[0]
	if list.Tool != "list_change_reviews" || list.Args["pr_url"] !=
		"https://github.com/o/r/pull/7" {
		t.Fatalf("the status job finds the review through pg_sage, not the comment: %+v", list)
	}
	e = githubEnv(t, g, `{"schedule":"*/15 * * * *"}`)
	if code := e.run(t, "status", "--github", "--url", m.srv.URL, "--database", "d"); code != 0 {
		t.Fatalf("second status job: exit %d", code)
	}
	_, _, again := g.snapshot()
	if len(again) != len(writes) {
		t.Fatalf("an unchanged review must not be re-posted: %v then %v", writes, again)
	}
}

func TestGitHubModeSkipsDocsOnlyAndForkPRsWithoutAToken(t *testing.T) {
	g := newFakeGitHub(t)
	m := newCRMCP(t, verdictTools("risky"))
	e := githubEnv(t, g, crPREvent)
	diff := writeFile(t, t.TempDir(), "pr.diff", crDocsDiff)
	if code := e.run(t, "submit", "--github", "--url", m.srv.URL, "--database", "d",
		"--diff-file", diff); code != 0 {
		t.Fatalf("docs-only: exit %d", code)
	}
	_, statuses, _ := g.snapshot()
	if len(statuses) != 1 || statuses[0].State != "success" ||
		!strings.Contains(statuses[0].Description, "no database-touching changes") {
		t.Fatalf("docs-only status %+v", statuses)
	}
	fork := strings.Replace(crPREvent, `"fork":false`, `"fork":true`, 1)
	e = githubEnv(t, g, fork)
	delete(e.vars, "PG_SAGE_TOKEN_FILE")
	sql := writeFile(t, t.TempDir(), "a.sql", "SELECT 1")
	if code := e.run(t, "submit", "--github", "--url", m.srv.URL, "--database", "d",
		"--sql-file", sql); code != 0 || !strings.Contains(e.stdout.String(), "::notice::") {
		t.Fatalf("fork without a token: exit %d %q, want 0 with a notice", code, e.stdout)
	}
	if len(m.requests()) != 0 {
		t.Fatal("neither skip path may call pg_sage")
	}
}

func TestGitHubModeNeedsItsEnvironment(t *testing.T) {
	g := newFakeGitHub(t)
	m := newCRMCP(t, verdictTools("safe"))
	sql := writeFile(t, t.TempDir(), "a.sql", "SELECT 1")
	for _, missing := range []string{"GITHUB_TOKEN", "GITHUB_REPOSITORY", "GITHUB_EVENT_PATH"} {
		e := githubEnv(t, g, crPREvent)
		delete(e.vars, missing)
		if code := e.run(t, "submit", "--github", "--url", m.srv.URL, "--database", "d",
			"--sql-file", sql); code != 2 || !strings.Contains(e.stderr.String(), missing) {
			t.Errorf("without %s: exit %d %q, want 2 naming it", missing, code, e.stderr)
		}
	}
	e := githubEnv(t, g, `{"pull_request":`)
	if code := e.run(t, "submit", "--github", "--url", m.srv.URL, "--database", "d",
		"--sql-file", sql); code != 2 {
		t.Fatalf("malformed event: exit %d, want 2", code)
	}
	g.srv.Close()
	e = githubEnv(t, g, crPREvent)
	if code := e.run(t, "submit", "--github", "--url", m.srv.URL, "--database", "d",
		"--sql-file", sql); code != 3 {
		t.Fatalf("GitHub unreachable: exit %d, want 3", code)
	}
}
