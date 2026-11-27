package llmstub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Mode is how the stub answers one chat-completions request.
type Mode string

const (
	// ModeValid returns the rule body verbatim as the completion.
	ModeValid Mode = "valid"
	// ModeFencedJSON wraps the body in a ```json fence.
	ModeFencedJSON Mode = "fenced_json"
	// ModeProseJSON surrounds the body with explanatory prose.
	ModeProseJSON Mode = "prose_json"
	// ModeNonJSON answers with prose that holds no JSON at all.
	ModeNonJSON Mode = "non_json"
	// ModeEmpty answers 200 with a blank completion.
	ModeEmpty Mode = "empty"
	// ModeRateLimited answers 429 with a Retry-After header.
	ModeRateLimited Mode = "rate_limited"
	// ModeTimeout holds the request past the client's deadline.
	ModeTimeout Mode = "timeout"
	// ModeMalformedToolCall returns a tool call whose arguments are not JSON.
	ModeMalformedToolCall Mode = "malformed_tool_call"
)

// NonJSONText is the completion ModeNonJSON returns.
const NonJSONText = "I reviewed the context and have no structured output to give."

// MalformedArguments is the tool-call arguments ModeMalformedToolCall returns.
const MalformedArguments = `{"sql": "SELECT 1"`

// Rule answers every request whose system or user prompt contains Match.
type Rule struct {
	Match string
	Mode  Mode
	Body  string
}

// Options scripts the stub. Zero values take the documented defaults.
type Options struct {
	// Rules are tried in order; the first match answers the request.
	Rules []Rule
	// Default answers requests no rule matches (zero: ModeValid).
	Default Mode
	// DefaultBody is the body for unmatched requests (zero: "[]").
	DefaultBody string
	// TotalTokens is the usage.total_tokens reported (zero: 50).
	TotalTokens int
	// RetryAfterSeconds is the Retry-After header of a 429 (zero: 1).
	RetryAfterSeconds int
	// Delay is how long ModeTimeout holds a request (zero: 30s).
	Delay time.Duration
	// ToolName names ModeMalformedToolCall's call (zero: "run_query").
	ToolName string
}

// Request is one recorded chat-completions request.
type Request struct {
	Path          string
	Authorization string
	Model         string
	System        string
	User          string
	HasTools      bool
	Mode          Mode
	At            time.Time
}

// Prompt is the system and user prompt joined, for substring checks.
func (r Request) Prompt() string { return r.System + "\n" + r.User }

// Server is a running stub. URL is the base to use as llm.endpoint.
type Server struct {
	URL  string
	srv  *httptest.Server
	opts Options

	mu       sync.Mutex
	requests []Request
	models   int
}

// New starts a stub and closes it when the test ends.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	s := Start(opts)
	t.Cleanup(s.Close)
	return s
}

// Start starts a stub the caller must Close.
func Start(opts Options) *Server {
	s := &Server{opts: withDefaults(opts)}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	s.URL = s.srv.URL + "/v1"
	return s
}

// Close stops the stub. Held ModeTimeout requests are released first.
func (s *Server) Close() {
	s.srv.CloseClientConnections()
	s.srv.Close()
}

func withDefaults(o Options) Options {
	if o.Default == "" {
		o.Default = ModeValid
	}
	if o.DefaultBody == "" {
		o.DefaultBody = "[]"
	}
	if o.TotalTokens <= 0 {
		o.TotalTokens = 50
	}
	if o.RetryAfterSeconds <= 0 {
		o.RetryAfterSeconds = 1
	}
	if o.Delay <= 0 {
		o.Delay = 30 * time.Second
	}
	if o.ToolName == "" {
		o.ToolName = "run_query"
	}
	return o
}

// Requests returns a copy of every recorded chat request, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Count is the number of chat requests received.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// CountMatching is the number of chat requests whose prompt contains text.
func (s *Server) CountMatching(text string) int {
	n := 0
	for _, r := range s.Requests() {
		if strings.Contains(r.Prompt(), text) {
			n++
		}
	}
	return n
}

// ModelListings is the number of GET .../models requests received.
func (s *Server) ModelListings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models
}

type chatRequest struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
		s.serveModels(w)
	case r.Method == http.MethodPost &&
		strings.HasSuffix(r.URL.Path, "/chat/completions"):
		s.serveChat(w, r)
	default:
		http.Error(w, `{"error":{"message":"llmstub: no such route"}}`,
			http.StatusNotFound)
	}
}

func (s *Server) serveModels(w http.ResponseWriter) {
	s.mu.Lock()
	s.models++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"stub-model"}]}`)
}

func (s *Server) serveChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, `{"error":{"message":"llmstub: read body"}}`,
			http.StatusBadRequest)
		return
	}
	req, err := decodeRequest(body)
	if err != nil {
		http.Error(w, `{"error":{"message":"llmstub: body is not a chat request"}}`,
			http.StatusBadRequest)
		return
	}
	req.Path = r.URL.Path
	req.Authorization = r.Header.Get("Authorization")
	req.At = time.Now()
	rule := s.match(req.Prompt())
	req.Mode = rule.Mode
	s.record(req)
	s.answer(w, r, rule)
}

func decodeRequest(body []byte) (Request, error) {
	var raw chatRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return Request{}, err
	}
	req := Request{Model: raw.Model, HasTools: len(raw.Tools) > 0}
	for _, m := range raw.Messages {
		var msg chatMessage
		// Non-string content (tool results, parts) is skipped, not fatal.
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		switch msg.Role {
		case "system":
			req.System += msg.Content
		case "user":
			req.User += msg.Content
		}
	}
	return req, nil
}

func (s *Server) match(prompt string) Rule {
	for _, rule := range s.opts.Rules {
		if rule.Match != "" && strings.Contains(prompt, rule.Match) {
			if rule.Mode == "" {
				rule.Mode = ModeValid
			}
			return rule
		}
	}
	return Rule{Mode: s.opts.Default, Body: s.opts.DefaultBody}
}

func (s *Server) record(req Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
}

func (s *Server) answer(w http.ResponseWriter, r *http.Request, rule Rule) {
	switch rule.Mode {
	case ModeRateLimited:
		w.Header().Set("Retry-After", strconv.Itoa(s.opts.RetryAfterSeconds))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited by llmstub",`+
			`"type":"rate_limit_error"}}`)
		return
	case ModeTimeout:
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.opts.Delay):
		}
	case ModeMalformedToolCall:
		s.writeToolCall(w)
		return
	}
	s.writeCompletion(w, Content(rule.Mode, rule.Body))
}

// Content is the completion text a mode produces for body.
func Content(mode Mode, body string) string {
	switch mode {
	case ModeFencedJSON:
		return "```json\n" + body + "\n```"
	case ModeProseJSON:
		return "Here is my analysis of the context.\n" + body +
			"\nThese changes should be reviewed before applying."
	case ModeNonJSON:
		return NonJSONText
	case ModeEmpty:
		return ""
	default:
		return body
	}
}

func (s *Server) writeCompletion(w http.ResponseWriter, content string) {
	resp := map[string]any{
		"id": "llmstub", "object": "chat.completion", "model": "stub-model",
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"total_tokens": s.opts.TotalTokens},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) writeToolCall(w http.ResponseWriter) {
	call := map[string]any{
		"id": "call_llmstub", "type": "function",
		"function": map[string]any{"name": s.opts.ToolName, "arguments": MalformedArguments},
	}
	resp := map[string]any{
		"id": "llmstub", "object": "chat.completion", "model": "stub-model",
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{"role": "assistant", "content": nil,
				"tool_calls": []any{call}},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]int{"total_tokens": s.opts.TotalTokens},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
