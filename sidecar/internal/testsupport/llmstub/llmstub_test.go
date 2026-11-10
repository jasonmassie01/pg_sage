package llmstub_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/testsupport/llmstub"
)

const arrayBody = `[{"object_identifier":"instance","severity":"info"}]`

func postChat(t *testing.T, url, system, user string) (*http.Response, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"model": "m1",
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/chat/completions",
		strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer stub-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post chat: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, string(body)
}

func completionContent(t *testing.T, body string) string {
	t.Helper()
	var decoded struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode completion %q: %v", body, err)
	}
	if len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content == nil {
		t.Fatalf("completion has no content: %s", body)
	}
	return *decoded.Choices[0].Message.Content
}

func TestValidModeRecordsRequestAndReturnsBody(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{Rules: []llmstub.Rule{
		{Match: "connection expert", Mode: llmstub.ModeValid, Body: arrayBody},
	}})
	resp, body := postChat(t, stub.URL, "You are a connection expert", "db=orders")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := completionContent(t, body); got != arrayBody {
		t.Fatalf("content = %q, want %q", got, arrayBody)
	}
	reqs := stub.Requests()
	if len(reqs) != 1 {
		t.Fatalf("recorded %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/v1/chat/completions" || r.Authorization != "Bearer stub-key" ||
		r.Model != "m1" || r.System != "You are a connection expert" ||
		r.User != "db=orders" || r.Mode != llmstub.ModeValid || r.At.IsZero() {
		t.Fatalf("recorded request = %+v", r)
	}
	if stub.CountMatching("db=orders") != 1 || stub.CountMatching("db=billing") != 0 {
		t.Fatalf("CountMatching wrong: orders=%d billing=%d",
			stub.CountMatching("db=orders"), stub.CountMatching("db=billing"))
	}
}

func TestZeroOptionsTakeDefaults(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{})
	_, body := postChat(t, stub.URL, "sys", "user")
	if got := completionContent(t, body); got != "[]" {
		t.Fatalf("default content = %q, want []", got)
	}
	if !strings.Contains(body, `"total_tokens":50`) {
		t.Fatalf("default usage missing from %s", body)
	}
}

func TestContentWrappingModes(t *testing.T) {
	cases := []struct {
		mode     llmstub.Mode
		parsable bool
	}{
		{llmstub.ModeValid, true},
		{llmstub.ModeFencedJSON, true},
		{llmstub.ModeProseJSON, true},
		{llmstub.ModeNonJSON, false},
		{llmstub.ModeEmpty, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			content := llmstub.Content(tc.mode, arrayBody)
			var recs []map[string]any
			err := llm.ParseJSON(content, llm.JSONArray, &recs)
			if tc.parsable && (err != nil || len(recs) != 1 ||
				recs[0]["object_identifier"] != "instance") {
				t.Fatalf("%s content %q: recs=%v err=%v", tc.mode, content, recs, err)
			}
			if !tc.parsable && err == nil {
				t.Fatalf("%s content %q parsed, want an error", tc.mode, content)
			}
		})
	}
	if llmstub.Content(llmstub.ModeFencedJSON, "[]") != "```json\n[]\n```" {
		t.Fatal("fenced mode does not use a ```json fence")
	}
}

func stubClient(url string, timeoutSeconds int) *llm.Client {
	return llm.New(&config.LLMConfig{
		Enabled: true, Endpoint: url, APIKey: "stub-key", Model: "m1",
		TimeoutSeconds: timeoutSeconds, TokenBudgetDaily: 100000,
	}, func(string, string, ...any) {})
}

// The real client treats a blank completion as a failed call (G3-B25).
func TestEmptyModeIsAFailedCallForTheRealClient(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{Default: llmstub.ModeEmpty, TotalTokens: 7})
	_, tokens, err := stubClient(stub.URL, 5).Chat(context.Background(), "s", "u", 64)
	if !errors.Is(err, llm.ErrEmptyResponse) {
		t.Fatalf("err = %v, want ErrEmptyResponse", err)
	}
	if tokens != 7 {
		t.Fatalf("tokens = %d, want the 7 the stub reported", tokens)
	}
}

func TestRateLimitedModeSendsRetryAfter(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{
		Default: llmstub.ModeRateLimited, RetryAfterSeconds: 2,
	})
	resp, body := postChat(t, stub.URL, "s", "u")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if !strings.Contains(body, "rate_limit_error") {
		t.Fatalf("429 body = %s", body)
	}
	if stub.Count() != 1 {
		t.Fatalf("count = %d, want 1 (a 429 is still recorded)", stub.Count())
	}
}

func TestTimeoutModeOutlastsTheClientDeadline(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{
		Default: llmstub.ModeTimeout, Delay: 20 * time.Second,
	})
	start := time.Now()
	_, _, err := stubClient(stub.URL, 1).Chat(context.Background(), "s", "u", 64)
	if err == nil {
		t.Fatal("Chat succeeded against a held request, want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Chat took %s: the client deadline did not end the call", elapsed)
	}
	if stub.Count() != 1 {
		t.Fatalf("count = %d, want 1", stub.Count())
	}
}

func TestTimeoutModeAnswersAfterDelay(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{
		Rules: []llmstub.Rule{{Match: "slow", Mode: llmstub.ModeTimeout, Body: "late"}},
		Delay: 50 * time.Millisecond,
	})
	_, body := postChat(t, stub.URL, "slow", "u")
	if got := completionContent(t, body); got != "late" {
		t.Fatalf("content after delay = %q, want late", got)
	}
}

func TestMalformedToolCallHasUnparsableArguments(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{Default: llmstub.ModeMalformedToolCall})
	_, body := postChat(t, stub.URL, "s", "u")
	var decoded struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Choices) != 1 || len(decoded.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("want one tool call, got %s", body)
	}
	call := decoded.Choices[0].Message.ToolCalls[0].Function
	if call.Name != "run_query" || decoded.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("tool call = %+v finish=%q", call, decoded.Choices[0].FinishReason)
	}
	if json.Valid([]byte(call.Arguments)) {
		t.Fatalf("arguments %q are valid JSON, want malformed", call.Arguments)
	}
}

func TestInvalidRequestsAreRejectedAndNotRecorded(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{})
	resp, err := http.Post(stub.URL+"/chat/completions", "application/json",
		strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-JSON body status = %d, want 400", resp.StatusCode)
	}
	resp, err = http.Get(stub.URL + "/embeddings")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want 404", resp.StatusCode)
	}
	if stub.Count() != 0 {
		t.Fatalf("count = %d, want 0 for rejected requests", stub.Count())
	}
}

func TestModelListingIsCountedSeparately(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{})
	resp, err := http.Get(stub.URL + "/models")
	if err != nil {
		t.Fatalf("get models: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "stub-model") {
		t.Fatalf("models body = %s", body)
	}
	if stub.ModelListings() != 1 || stub.Count() != 0 {
		t.Fatalf("listings=%d chats=%d, want 1 and 0", stub.ModelListings(), stub.Count())
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{Rules: []llmstub.Rule{
		{Match: "wal", Mode: llmstub.ModeNonJSON},
		{Match: "wal", Mode: llmstub.ModeValid, Body: arrayBody},
		{Match: "", Mode: llmstub.ModeEmpty},
	}})
	_, body := postChat(t, stub.URL, "wal expert", "u")
	if got := completionContent(t, body); got != llmstub.NonJSONText {
		t.Fatalf("content = %q, want the first rule's", got)
	}
	_, body = postChat(t, stub.URL, "other", "u")
	if got := completionContent(t, body); got != "[]" {
		t.Fatalf("an empty Match must never match; content = %q", got)
	}
}

func TestConcurrentRequestsAreAllRecorded(t *testing.T) {
	stub := llmstub.New(t, llmstub.Options{})
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"messages":[{"role":"user","content":"marker-%02d"}]}`, i)
			resp, err := http.Post(stub.URL+"/chat/completions", "application/json",
				strings.NewReader(body))
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent post: %v", err)
	}
	if stub.Count() != n {
		t.Fatalf("count = %d, want %d", stub.Count(), n)
	}
	for i := 0; i < n; i++ {
		if got := stub.CountMatching(fmt.Sprintf("marker-%02d", i)); got != 1 {
			t.Fatalf("marker-%02d recorded %d times, want 1", i, got)
		}
	}
}
