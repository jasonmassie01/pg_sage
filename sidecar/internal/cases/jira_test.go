package cases

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// market-14: the Jira Cloud REST API v3 client behind "Create ticket".
// No concurrent access tests: the client holds only immutable settings
// and an http.Client, which is safe for concurrent use.

type jiraHit struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

type jiraFake struct {
	mu     sync.Mutex
	hits   []jiraHit
	status int
	reply  string
}

func newJiraFake(t *testing.T, status int, reply string) (*jiraFake, *JiraClient) {
	t.Helper()
	f := &jiraFake{status: status, reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		h := jiraHit{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization")}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &h.Body)
		}
		f.mu.Lock()
		f.hits = append(f.hits, h)
		status, reply := f.status, f.reply
		f.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	c, err := NewJiraClient(JiraConfig{BaseURL: srv.URL + "/", Email: "bot@example.com",
		APIToken: "jira-token-1", ProjectKey: "OPS"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

const jiraGoldenCreate = `{"fields": {
 "project": {"key": "OPS"},
 "issuetype": {"name": "Task"},
 "summary": "[pg_sage] db1: Table t is bloated",
 "labels": ["pg_sage"],
 "description": {"type": "doc", "version": 1, "content": [
  {"type": "paragraph", "content": [{"type": "text", "text": "line one\nline two"}]},
  {"type": "paragraph", "content": [{"type": "text", "text": "Open the case in pg_sage",
   "marks": [{"type": "link", "attrs": {"href": "https://sage.example.com/cases?case=x"}}]}]}
 ]}
}}`

func TestJiraCreateIssue_GoldenPayloadAndBasicAuth(t *testing.T) {
	f, c := newJiraFake(t, http.StatusCreated, `{"id":"10001","key":"OPS-12"}`)
	ref, err := c.CreateIssue(context.Background(), IssueRequest{
		Summary: "[pg_sage] db1: Table t is bloated", Description: "line one\nline two",
		CaseURL: "https://sage.example.com/cases?case=x"})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if ref.Key != "OPS-12" || !strings.HasSuffix(ref.URL, "/browse/OPS-12") ||
		strings.Contains(ref.URL, "//browse") {
		t.Fatalf("ref = %+v", ref)
	}
	h := f.hits[0]
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@example.com:jira-token-1"))
	if h.Method != http.MethodPost || h.Path != "/rest/api/3/issue" || h.Auth != wantAuth {
		t.Fatalf("hit = %+v", h)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(jiraGoldenCreate), &want)
	if !reflect.DeepEqual(h.Body, want) {
		got, _ := json.MarshalIndent(h.Body, "", " ")
		t.Fatalf("payload\n got %s\nwant %s", got, jiraGoldenCreate)
	}
}

func TestJiraCreateIssue_WithoutCaseURLHasNoLink(t *testing.T) {
	f, c := newJiraFake(t, http.StatusCreated, `{"key":"OPS-1"}`)
	if _, err := c.CreateIssue(context.Background(), IssueRequest{Summary: "s"}); err != nil {
		t.Fatal(err)
	}
	doc := f.hits[0].Body["fields"].(map[string]any)["description"].(map[string]any)
	if n := len(doc["content"].([]any)); n != 1 {
		t.Fatalf("description paragraphs = %d, want 1 (no link)", n)
	}
}

func TestJiraIssueStatus_ReadsNameAndCategory(t *testing.T) {
	f, c := newJiraFake(t, http.StatusOK,
		`{"fields":{"status":{"name":"In Progress","statusCategory":{"key":"indeterminate"}}}}`)
	st, err := c.IssueStatus(context.Background(), "OPS-12")
	if err != nil || st != (IssueStatus{Name: "In Progress", Category: "indeterminate"}) {
		t.Fatalf("IssueStatus = %+v, %v", st, err)
	}
	if h := f.hits[0]; h.Method != http.MethodGet || h.Path != "/rest/api/3/issue/OPS-12" ||
		h.Query != "fields=status" {
		t.Fatalf("hit = %+v", h)
	}
}

func TestJiraAddComment_PostsADF(t *testing.T) {
	f, c := newJiraFake(t, http.StatusCreated, `{}`)
	if err := c.AddComment(context.Background(), "OPS-12", "resolved"); err != nil {
		t.Fatal(err)
	}
	h := f.hits[0]
	body, _ := h.Body["body"].(map[string]any)
	if h.Path != "/rest/api/3/issue/OPS-12/comment" || body["type"] != "doc" {
		t.Fatalf("hit = %+v", h)
	}
}

func TestJiraErrorsAreTyped(t *testing.T) {
	cases := map[int]error{401: ErrTicketUnauthorized, 403: ErrTicketUnauthorized,
		404: ErrTicketNotFound}
	for status, want := range cases {
		_, c := newJiraFake(t, status, `{"errorMessages":["no"]}`)
		if _, err := c.IssueStatus(context.Background(), "OPS-1"); !errors.Is(err, want) {
			t.Errorf("status %d: err = %v, want %v", status, err, want)
		}
	}
	_, c := newJiraFake(t, http.StatusInternalServerError, `oops`)
	_, err := c.CreateIssue(context.Background(), IssueRequest{Summary: "s"})
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(),
		"jira-token-1") {
		t.Fatalf("500 err = %v, want the status and no token", err)
	}
	_, c = newJiraFake(t, http.StatusCreated, `not json`)
	if _, err := c.CreateIssue(context.Background(), IssueRequest{Summary: "s"}); err == nil {
		t.Fatal("a non-JSON create answer was accepted")
	}
	_, c = newJiraFake(t, http.StatusCreated, `{"key":"../../x"}`)
	if _, err := c.CreateIssue(context.Background(), IssueRequest{Summary: "s"}); err == nil {
		t.Fatal("a malformed issue key from Jira was accepted")
	}
}

func TestJiraRefusesMalformedIssueKeys(t *testing.T) {
	f, c := newJiraFake(t, http.StatusOK, `{}`)
	for _, key := range []string{"", "OPS", "ops-1", "OPS-1/../2", "OPS-1?x=1", "-1"} {
		if _, err := c.IssueStatus(context.Background(), key); !errors.Is(err, ErrTicketInvalid) {
			t.Errorf("IssueStatus(%q) = %v, want ErrTicketInvalid", key, err)
		}
	}
	if len(f.hits) != 0 {
		t.Fatalf("a malformed key reached Jira %d time(s)", len(f.hits))
	}
}

func TestNewJiraClient_ValidatesConfig(t *testing.T) {
	good := JiraConfig{BaseURL: "https://acme.atlassian.net", Email: "e", APIToken: "t",
		ProjectKey: "OPS"}
	if _, err := NewJiraClient(good, http.DefaultClient); err != nil {
		t.Fatalf("good config: %v", err)
	}
	for name, mutate := range map[string]func(*JiraConfig){
		"no base":    func(c *JiraConfig) { c.BaseURL = "" },
		"no email":   func(c *JiraConfig) { c.Email = "" },
		"no token":   func(c *JiraConfig) { c.APIToken = "" },
		"no project": func(c *JiraConfig) { c.ProjectKey = "" },
		"nil client": nil,
	} {
		cfg := good
		client := http.DefaultClient
		if mutate == nil {
			client = nil
		} else {
			mutate(&cfg)
		}
		if _, err := NewJiraClient(cfg, client); !errors.Is(err, ErrTicketInvalid) {
			t.Errorf("%s: err = %v, want ErrTicketInvalid", name, err)
		}
	}
}
