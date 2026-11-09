package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// step is one scripted provider reply. A zero status means 200 with body,
// or with a valid chat completion when body is empty.
type step struct {
	status     int
	retryAfter string
	body       string
	delay      time.Duration
}

// scriptServer answers requests with its steps in order and repeats the
// last step once the script runs out. It never reaches a live provider.
type scriptServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	steps []step
	at    []time.Time
	calls atomic.Int32
}

func newScriptServer(t *testing.T, steps ...step) *scriptServer {
	t.Helper()
	s := &scriptServer{steps: steps}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scriptServer) handle(w http.ResponseWriter, r *http.Request) {
	n := int(s.calls.Add(1)) - 1
	_, _ = io.Copy(io.Discard, r.Body)
	s.mu.Lock()
	s.at = append(s.at, time.Now())
	st := step{}
	if len(s.steps) > 0 {
		st = s.steps[min(n, len(s.steps)-1)]
	}
	s.mu.Unlock()
	if st.delay > 0 {
		select {
		case <-time.After(st.delay):
		case <-r.Context().Done():
			return
		}
	}
	if st.retryAfter != "" {
		w.Header().Set("Retry-After", st.retryAfter)
	}
	if st.status == 0 || st.status == http.StatusOK {
		if st.body != "" {
			_, _ = w.Write([]byte(st.body))
			return
		}
		_, _ = w.Write(testChatJSON("ok", 10))
		return
	}
	w.WriteHeader(st.status)
	body := st.body
	if body == "" {
		body = `{"error":{"message":"scripted failure"}}`
	}
	_, _ = w.Write([]byte(body))
}

// gap is the time between request i-1 and request i.
func (s *scriptServer) gap(i int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i <= 0 || i >= len(s.at) {
		return 0
	}
	return s.at[i].Sub(s.at[i-1])
}

// scriptClient is a client for s with a zero retry ladder, so retries
// are immediate unless the provider sends Retry-After.
func scriptClient(s *scriptServer, model string, timeoutSeconds int) *Client {
	c := New(&config.LLMConfig{
		Enabled: true, Endpoint: s.srv.URL, APIKey: "k", Model: model,
		TimeoutSeconds: timeoutSeconds, TokenBudgetDaily: 10_000_000,
		CooldownSeconds: 60,
	}, noopLog)
	c.retryDelays = []time.Duration{0, 0, 0}
	return c
}

// recordFailure counts one generic provider failure, as the pre-llm-02
// breaker did for every failure; older tests use it to open the breaker.
func (c *Client) recordFailure() { c.recordProviderFailure(classServer) }

func circuitState(c *Client) string { return StatusOf(c).CircuitState }
