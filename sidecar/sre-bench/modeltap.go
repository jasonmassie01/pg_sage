package srebench

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// The model tap: an in-process OpenAI-compatible proxy between the
// investigator's LLM client and the model (the fake, or a live
// endpoint). It forwards every request unchanged and records the two
// things the bench grades about the data flow: the message text that
// left the process (canary and redaction checks) and the provider's
// token usage (cost). Headers, including Authorization, are forwarded
// and never recorded.

// Tap limits.
const (
	tapMaxBody = 4 << 20
	tapTimeout = 90 * time.Second
)

// TapUsage is the model traffic of one run (or a sum of runs).
type TapUsage struct {
	Calls            int `json:"calls"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
	HTTPErrors       int `json:"http_errors"`
}

// Add sums o into u.
func (u *TapUsage) Add(o TapUsage) {
	u.Calls += o.Calls
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.HTTPErrors += o.HTTPErrors
}

// ModelTap is one run's recording proxy.
type ModelTap struct {
	upstream string
	srv      *httptest.Server
	client   *http.Client
	pace     *pacer // nil: unpaced

	mu      sync.Mutex
	usage   TapUsage
	prompts []string
}

// NewModelTap starts a tap forwarding to upstream, an OpenAI-compatible
// base URL (the request path, e.g. /chat/completions, is appended).
func NewModelTap(upstream string) *ModelTap {
	t := &ModelTap{upstream: strings.TrimRight(upstream, "/"),
		client: &http.Client{Timeout: tapTimeout}}
	t.srv = httptest.NewServer(t)
	return t
}

// URL is the tap's endpoint.
func (t *ModelTap) URL() string { return t.srv.URL }

// Close stops the tap.
func (t *ModelTap) Close() { t.srv.Close() }

// Usage is the traffic recorded so far.
func (t *ModelTap) Usage() TapUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usage
}

// Prompts is every message text sent so far, in order.
func (t *ModelTap) Prompts() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.prompts...)
}

// ServeHTTP forwards one request and records it.
func (t *ModelTap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, tapMaxBody))
	t.record(body)
	if err != nil {
		t.fail(w, http.StatusBadRequest)
		return
	}
	if err := t.pace.wait(r.Context()); err != nil {
		t.fail(w, http.StatusServiceUnavailable)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, t.upstream+r.URL.Path,
		bytes.NewReader(body))
	if err != nil {
		t.fail(w, http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Authorization", "Content-Type", "Accept"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := t.client.Do(req)
	if err != nil {
		// The error names the upstream URL only; it never carries headers.
		t.fail(w, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, tapMaxBody))
	if err != nil {
		t.fail(w, http.StatusBadGateway)
		return
	}
	t.settle(resp.StatusCode, reply)
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(reply)
}

func (t *ModelTap) fail(w http.ResponseWriter, status int) {
	t.mu.Lock()
	t.usage.HTTPErrors++
	t.mu.Unlock()
	http.Error(w, `{"error":{"message":"model tap: upstream unavailable"}}`, status)
}

// record counts the call and keeps its message text.
func (t *ModelTap) record(body []byte) {
	var req struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req) // a malformed request is still a call
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usage.Calls++
	for _, m := range req.Messages {
		switch c := m.Content.(type) {
		case string:
			t.prompts = append(t.prompts, c)
		case nil:
		default:
			raw, _ := json.Marshal(c)
			t.prompts = append(t.prompts, string(raw))
		}
	}
}

// settle records the reply's status and token usage. Gemini reports
// thinking only as total above prompt plus completion.
func (t *ModelTap) settle(status int, reply []byte) {
	var r struct {
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
			Details    *struct {
				Reasoning int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	parsed := json.Unmarshal(reply, &r) == nil
	t.mu.Lock()
	defer t.mu.Unlock()
	if status >= 300 {
		t.usage.HTTPErrors++
	}
	if !parsed {
		return
	}
	u := r.Usage
	t.usage.PromptTokens += u.Prompt
	t.usage.CompletionTokens += u.Completion
	switch {
	case u.Details != nil && u.Details.Reasoning > 0:
		t.usage.ReasoningTokens += u.Details.Reasoning
	case u.Total > u.Prompt+u.Completion:
		t.usage.ReasoningTokens += u.Total - u.Prompt - u.Completion
	}
}
