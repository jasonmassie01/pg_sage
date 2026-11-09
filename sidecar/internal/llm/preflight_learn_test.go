package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// llm-05: limits are learned from model listings, which only Gemini's
// native listing fills. No test here reaches a real provider: every
// request goes through a fake transport.

const geminiTestEndpoint = "https://generativelanguage.googleapis.com/v1beta/openai/"

const geminiListing = `{"models":[{"name":"models/gemini-x","displayName":"X",` +
	`"inputTokenLimit":2000,"outputTokenLimit":100,` +
	`"supportedGenerationMethods":["generateContent"]}]}`

// fakeTransport answers listings with listing and counts every request;
// chat requests (anything not a listing) also answer "ok".
type fakeTransport struct {
	listings, chats atomic.Int64
	listing         string
	listingStatus   int
	block           bool
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.block {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	body, status := `{"choices":[{"message":{"content":"ok"},`+
		`"finish_reason":"stop"}],"usage":{"total_tokens":3}}`, http.StatusOK
	if req.Method == http.MethodGet && strings.Contains(req.URL.Path, "models") {
		f.listings.Add(1)
		body, status = f.listing, f.listingStatus
		if status == 0 {
			status = http.StatusOK
		}
	} else {
		f.chats.Add(1)
	}
	return &http.Response{StatusCode: status, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func geminiClient(t *testing.T, ft *fakeTransport, logs *[]string) *Client {
	t.Helper()
	logFn := func(_, msg string, args ...any) {
		if logs != nil {
			*logs = append(*logs, msg)
		}
	}
	c := New(&config.LLMConfig{Enabled: true, Endpoint: geminiTestEndpoint,
		APIKey: "learn-" + t.Name(), Model: "models/gemini-x", TimeoutSeconds: 5,
		TokenBudgetDaily: 1000000}, logFn)
	c.httpClient = &http.Client{Transport: ft}
	return c
}

func TestListModelsFeedsTheGuard(t *testing.T) {
	ft := &fakeTransport{listing: geminiListing}
	c := geminiClient(t, ft, nil)
	if _, err := ListModelsWithClient(context.Background(), geminiTestEndpoint,
		"learn-"+t.Name(), &http.Client{Transport: ft}); err != nil {
		t.Fatalf("ListModelsWithClient: %v", err)
	}
	if ft.listings.Load() != 1 {
		t.Fatalf("listings = %d, want 1", ft.listings.Load())
	}
	_, _, err := c.Chat(context.Background(), "", promptOf(1901), 100)
	if !errors.Is(err, ErrPromptTooLarge) {
		t.Fatalf("err = %v, want the listed limit to guard the client", err)
	}
	if _, _, err := c.Chat(context.Background(), "", promptOf(1900), 100); err != nil {
		t.Fatalf("prompt at the listed bound: %v", err)
	}
	if ft.chats.Load() != 1 {
		t.Fatalf("chat requests = %d, want only the one under the bound", ft.chats.Load())
	}
}

func TestLearnInputLimits_GeminiListsOnce(t *testing.T) {
	ft := &fakeTransport{listing: geminiListing}
	c := geminiClient(t, ft, nil)
	c.LearnInputLimits(context.Background())
	if ft.listings.Load() != 1 {
		t.Fatalf("listings = %d, want 1", ft.listings.Load())
	}
	cfg, _ := c.configSnapshot()
	if got := inputLimits.limit(cfg); got != 2000 {
		t.Fatalf("learned limit = %d, want 2000", got)
	}
}

func TestLearnInputLimits_NoListingWhereLimitsAreNotReported(t *testing.T) {
	ft := &fakeTransport{listing: `{"data":[{"id":"m"}]}`}
	for name, cfg := range map[string]config.LLMConfig{
		"openai-compatible": {Enabled: true, Endpoint: "https://api.example/v1",
			APIKey: "k", Model: "m"},
		"disabled": {Enabled: false, Endpoint: geminiTestEndpoint, APIKey: "k",
			Model: "m"},
		"no key": {Enabled: true, Endpoint: geminiTestEndpoint, Model: "m"},
		"databricks": {Enabled: true, Auth: "databricks", Endpoint: geminiTestEndpoint,
			Model: "m"},
	} {
		c := New(&cfg, noopLog)
		c.httpClient = &http.Client{Transport: ft}
		c.LearnInputLimits(context.Background())
		if n := ft.listings.Load() + ft.chats.Load(); n != 0 {
			t.Fatalf("%s: %d requests, want none", name, n)
		}
	}
	var nilClient *Client
	nilClient.LearnInputLimits(context.Background())
}

func TestLearnInputLimits_FailureIsLoggedAndLeavesGuardInactive(t *testing.T) {
	ft := &fakeTransport{listing: `{"error":"denied"}`, listingStatus: http.StatusForbidden}
	var logs []string
	c := geminiClient(t, ft, &logs)
	c.LearnInputLimits(context.Background())
	if len(logs) != 1 || !strings.Contains(logs[0], "guard") {
		t.Fatalf("logs = %q, want one line saying the guard is inactive", logs)
	}
	if strings.Contains(strings.Join(logs, " "), "learn-") {
		t.Fatal("the failure log carries the API key")
	}
	if _, _, err := c.Chat(context.Background(), "", promptOf(100000), 100); err != nil {
		t.Fatalf("guard active after a failed listing: %v", err)
	}
	if ft.chats.Load() != 1 {
		t.Fatalf("chat requests = %d, want 1", ft.chats.Load())
	}
}

func TestLearnInputLimits_IsBounded(t *testing.T) {
	prev := inputLimitListingTimeout
	inputLimitListingTimeout = 50 * time.Millisecond
	t.Cleanup(func() { inputLimitListingTimeout = prev })
	c := geminiClient(t, &fakeTransport{block: true}, nil)
	start := time.Now()
	c.LearnInputLimits(context.Background())
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("listing took %s, want it cut at the timeout", took)
	}
}

func TestLearnInputLimits_DefaultTimeoutIsFiveSeconds(t *testing.T) {
	if inputLimitListingTimeout != 5*time.Second {
		t.Fatalf("listing timeout = %s, want 5s", inputLimitListingTimeout)
	}
}
