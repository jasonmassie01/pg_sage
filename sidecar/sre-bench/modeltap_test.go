package srebench

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The model tap sits between the investigator's LLM client and the
// model (fake or live). It forwards each chat completion unchanged and
// records what the bench needs to grade the data flow: the message text
// that left the process (canary and redaction checks) and the provider's
// token usage (cost). The Authorization header is forwarded and never
// recorded.

const tapKey = "test-key-never-recorded"

func upstream(t *testing.T, status int, body string, gotAuth *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization") + " " + r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func chat(t *testing.T, url, content string) (*http.Response, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "m", "messages": []map[string]any{
		{"role": "system", "content": "rules"}, {"role": "user", "content": content}}})
	req, _ := http.NewRequest(http.MethodPost, url+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tapKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func TestModelTap_ForwardsAndRecordsUsage(t *testing.T) {
	var auth atomic.Value
	reply := `{"choices":[{"message":{"role":"assistant","content":"{}"}}],` +
		`"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1700,` +
		`"completion_tokens_details":{"reasoning_tokens":150}}}`
	up := upstream(t, http.StatusOK, reply, &auth)
	tap := NewModelTap(up.URL + "/v1beta/openai/")
	t.Cleanup(tap.Close)
	resp, body := chat(t, tap.URL(), "evidence E1 password=hunter2")
	if resp.StatusCode != http.StatusOK || body != reply {
		t.Fatalf("tap answered %d %q, want the upstream reply", resp.StatusCode, body)
	}
	if got := auth.Load().(string); got != "Bearer "+tapKey+" /v1beta/openai/chat/completions" {
		t.Fatalf("upstream saw %q", got)
	}
	u := tap.Usage()
	if u.Calls != 1 || u.PromptTokens != 1200 || u.CompletionTokens != 300 ||
		u.ReasoningTokens != 150 || u.HTTPErrors != 0 {
		t.Fatalf("usage %+v", u)
	}
	prompts := strings.Join(tap.Prompts(), "\n")
	if !strings.Contains(prompts, "evidence E1 password=hunter2") ||
		!strings.Contains(prompts, "rules") {
		t.Fatalf("prompts %q miss the messages that left the process", prompts)
	}
	if strings.Contains(prompts, tapKey) {
		t.Fatal("the tap recorded the API key")
	}
}

// Gemini reports thinking as total above prompt + completion, without
// completion_tokens_details.
func TestModelTap_GeminiReasoningFromTotal(t *testing.T) {
	var auth atomic.Value
	up := upstream(t, http.StatusOK, `{"choices":[],"usage":{"prompt_tokens":100,`+
		`"completion_tokens":40,"total_tokens":540}}`, &auth)
	tap := NewModelTap(up.URL)
	t.Cleanup(tap.Close)
	chat(t, tap.URL(), "x")
	if u := tap.Usage(); u.ReasoningTokens != 400 || u.PromptTokens != 100 ||
		u.CompletionTokens != 40 {
		t.Fatalf("usage %+v, want reasoning 400", u)
	}
}

func TestModelTap_ErrorsPassThroughAndCount(t *testing.T) {
	var auth atomic.Value
	up := upstream(t, http.StatusTooManyRequests, `{"error":{"message":"quota"}}`, &auth)
	tap := NewModelTap(up.URL)
	t.Cleanup(tap.Close)
	resp, body := chat(t, tap.URL(), "x")
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "quota") {
		t.Fatalf("tap answered %d %q, want the upstream 429", resp.StatusCode, body)
	}
	if u := tap.Usage(); u.Calls != 1 || u.HTTPErrors != 1 || u.PromptTokens != 0 {
		t.Fatalf("usage %+v", u)
	}
}

func TestModelTap_UnreachableUpstreamIsABadGateway(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	tap := NewModelTap(url)
	t.Cleanup(tap.Close)
	resp, body := chat(t, tap.URL(), "x")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
	if strings.Contains(body, tapKey) {
		t.Fatal("the error reply carries the key")
	}
	if u := tap.Usage(); u.HTTPErrors != 1 || u.Calls != 1 {
		t.Fatalf("usage %+v", u)
	}
}

func TestModelTap_MalformedUsageIsZeroNotAFailure(t *testing.T) {
	var auth atomic.Value
	up := upstream(t, http.StatusOK, `not json`, &auth)
	tap := NewModelTap(up.URL)
	t.Cleanup(tap.Close)
	resp, body := chat(t, tap.URL(), "x")
	if resp.StatusCode != http.StatusOK || body != "not json" {
		t.Fatalf("tap must forward a malformed reply untouched: %d %q", resp.StatusCode, body)
	}
	if u := tap.Usage(); u.Calls != 1 || u.PromptTokens != 0 || u.HTTPErrors != 0 {
		t.Fatalf("usage %+v", u)
	}
}

func TestTapUsage_Add(t *testing.T) {
	a := TapUsage{Calls: 1, PromptTokens: 10, CompletionTokens: 2, ReasoningTokens: 3,
		HTTPErrors: 1}
	a.Add(TapUsage{Calls: 2, PromptTokens: 5, CompletionTokens: 1, ReasoningTokens: 0})
	if a != (TapUsage{Calls: 3, PromptTokens: 15, CompletionTokens: 3, ReasoningTokens: 3,
		HTTPErrors: 1}) {
		t.Fatalf("sum %+v", a)
	}
}
