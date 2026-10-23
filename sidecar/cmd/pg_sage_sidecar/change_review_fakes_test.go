package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test doubles for 'pg_sage review': a scripted MCP Streamable HTTP server
// (initialize, notifications/initialized, tools/call), a fake GitHub REST
// API, and an environment that records sleeps instead of sleeping.

const crTestToken = "pgs_live_TOKEN_must_never_leak_0123456789"

type crCall struct {
	Tool string
	Args map[string]any
}

// crMCP is a fake pg_sage MCP endpoint. tools answers each tools/call;
// a nil answer with a non-zero code is an isError result.
type crMCP struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []crCall
	raw    []string // every request: headers and body
	status int      // non-zero: answer every request with this HTTP status
	tools  func(tool string, args map[string]any) (map[string]any, int)
}

func newCRMCP(t *testing.T, tools func(string, map[string]any) (map[string]any, int)) *crMCP {
	t.Helper()
	m := &crMCP{tools: tools}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *crMCP) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var headers bytes.Buffer
	_ = r.Header.Write(&headers)
	m.mu.Lock()
	m.raw = append(m.raw, r.URL.String()+"\n"+headers.String()+"\n"+string(body))
	status := m.status
	m.mu.Unlock()
	if status != 0 {
		http.Error(w, `{"error":"refused for `+crTestToken+`"}`, status)
		return
	}
	if r.URL.Path != "/api/v1/mcp" || r.Header.Get("Authorization") != "Bearer "+crTestToken {
		http.Error(w, `{"code":"mcp_token_required"}`, http.StatusUnauthorized)
		return
	}
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	m.answer(w, msg.ID, msg.Method, msg.Params.Name, msg.Params.Arguments)
}

func (m *crMCP) answer(w http.ResponseWriter, id json.RawMessage, method, tool string,
	args map[string]any) {
	var result any
	switch method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18",
			"capabilities": map[string]any{},
			"serverInfo":   map[string]any{"name": "pg_sage", "version": "test"}}
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/call":
		m.mu.Lock()
		m.calls = append(m.calls, crCall{Tool: tool, Args: args})
		m.mu.Unlock()
		out, code := m.tools(tool, args)
		result = map[string]any{"structuredContent": out, "content": []any{}}
		if code != 0 {
			failed := map[string]any{"code": code, "message": "refused by the fake"}
			result = map[string]any{"isError": true, "content": []any{},
				"structuredContent": map[string]any{"error": failed}}
		}
	default:
		http.Error(w, "unknown method", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (m *crMCP) toolCalls() []crCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]crCall(nil), m.calls...)
}

func (m *crMCP) requests() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.raw, "\n----\n")
}

// crReview is a review as the MCP tools return it.
func crReview(id, state, verdict string) map[string]any {
	md := "<!-- pg_sage-review db=d id=" + id + " receipt=pending -->\n**pg_sage: " +
		verdict + "**\n"
	r := map[string]any{"id": id, "database": "d", "state": state, "verdict": verdict,
		"deterministic_verdict": verdict, "markdown": md, "shadow_score": "unscored",
		"credit_state": "none", "pr_url": "https://github.com/o/r/pull/7",
		"head_sha": "abcdef1", "summary": map[string]any{"critical": 0, "warning": 0},
		"created_at": "2026-10-23T10:00:00Z", "updated_at": "2026-10-23T10:00:00Z"}
	if state != "complete" {
		r["verdict"], r["deterministic_verdict"], r["markdown"] = "", "", ""
	}
	return r
}

const crReviewID = "3f0c8a52-1111-4000-8000-00000000000a"

// crEnv is a review environment over buffers with recorded sleeps.
type crEnv struct {
	env            reviewEnv
	stdout, stderr *bytes.Buffer
	vars           map[string]string
	sleeps         []time.Duration
	gitArgs        [][]string
}

func newCREnv(t *testing.T) *crEnv {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(crTestToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &crEnv{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		vars: map[string]string{"PG_SAGE_TOKEN_FILE": tokenFile}}
	e.env = reviewEnv{getenv: func(k string) string { return e.vars[k] },
		stdout: e.stdout, stderr: e.stderr, http: &http.Client{Timeout: 10 * time.Second},
		now: func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) },
		sleep: func(_ context.Context, d time.Duration) error {
			e.sleeps = append(e.sleeps, d)
			return nil
		},
		git: func(_ context.Context, args ...string) ([]byte, error) {
			e.gitArgs = append(e.gitArgs, args)
			return []byte(crSQLDiff), nil
		}}
	return e
}

func (e *crEnv) run(t *testing.T, args ...string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return runReviewCommand(ctx, args, e.env)
}

// writeFile writes content under dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// crSQLDiff changes one migration and the README.
const crSQLDiff = `diff --git a/db/migrate/002_add_note.sql b/db/migrate/002_add_note.sql
new file mode 100644
--- /dev/null
+++ b/db/migrate/002_add_note.sql
@@ -0,0 +1 @@
+ALTER TABLE orders ADD COLUMN note text;
diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-old
+new
`

// crDocsDiff touches no database file.
const crDocsDiff = `diff --git a/README.md b/README.md
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-old
+new
diff --git a/web/site.css b/web/site.css
--- a/web/site.css
+++ b/web/site.css
@@ -1 +1 @@
-a{}
+b{}
`
