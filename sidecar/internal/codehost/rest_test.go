package codehost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No state transition tests beyond the rate-limit cool-down below: the
// clients keep no other state between calls.

func TestBaseURLChecks(t *testing.T) {
	bad := map[string]string{
		"plain http":     "http://github.example.com/api/v3",
		"metadata IP":    "https://169.254.169.254/latest",
		"metadata name":  "https://metadata.google.internal/",
		"credentials":    "https://user:pw@github.example.com",
		"query":          "https://github.example.com/api?x=1",
		"relative":       "api.github.com",
		"unspecified IP": "https://0.0.0.0/",
	}
	for name, raw := range bad {
		_, err := NewGitHub(Options{BaseURL: raw})
		require.ErrorIs(t, err, ErrInvalid, name)
		_, err = NewGitLab(Options{BaseURL: raw})
		require.ErrorIs(t, err, ErrInvalid, name)
	}
	gh, err := NewGitHub(Options{})
	require.NoError(t, err)
	require.Equal(t, "github api.github.com", gh.Name())
	gl, err := NewGitLab(Options{BaseURL: "https://gitlab.example.com/api/v4/"})
	require.NoError(t, err)
	require.Equal(t, "gitlab gitlab.example.com", gl.Name())
	_, err = NewGitHub(Options{BaseURL: "http://localhost:8080"})
	require.NoError(t, err, "http is allowed on loopback for a local relay")
}

// echoServer answers every request with status and a message quoting the
// request's credentials, as a misbehaving proxy might.
func echoServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"rejected ` + r.Header.Get("Authorization") +
			r.Header.Get("PRIVATE-TOKEN") + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func everyCall(ctx context.Context, h Host, repo string) []error {
	req := PRRequest{Title: "t", Branch: "pg_sage/x", Files: []FileChange{{Path: "a.sql"}}}
	var errs []error
	_, err := h.SearchCode(ctx, repo, "orders")
	errs = append(errs, err)
	_, err = h.GetFile(ctx, repo, "a.rb")
	errs = append(errs, err)
	_, err = h.ListOpenPRs(ctx, repo, "pg_sage")
	errs = append(errs, err)
	_, err = h.OpenPR(ctx, repo, req)
	errs = append(errs, err)
	errs = append(errs, h.Comment(ctx, repo, 1, "x"))
	_, err = h.PRState(ctx, repo, 1)
	return append(errs, err)
}

func TestTokenIsAbsentFromEveryError(t *testing.T) {
	for _, status := range []int{401, 403, 404, 422, 500, 502} {
		srv := echoServer(t, status)
		gh, err := NewGitHub(Options{BaseURL: srv.URL, Token: testToken})
		require.NoError(t, err)
		gl, err := NewGitLab(Options{BaseURL: srv.URL, Token: testToken})
		require.NoError(t, err)
		ctx := context.Background()
		errs := append(everyCall(ctx, gh, "acme/shop"), everyCall(ctx, gl, "acme/shop")...)
		for i, err := range errs {
			require.Error(t, err, "status %d call %d", status, i)
			require.NotContains(t, err.Error(), testToken, "status %d call %d", status, i)
		}
		require.ErrorContains(t, errs[0], "[redacted]", "the echoed token is redacted")
	}
	// Transport failure: the server is gone.
	srv := echoServer(t, 200)
	srv.Close()
	gh, err := NewGitHub(Options{BaseURL: srv.URL, Token: testToken})
	require.NoError(t, err)
	for _, err := range everyCall(context.Background(), gh, "acme/shop") {
		require.ErrorIs(t, err, ErrUnavailable)
		require.NotContains(t, err.Error(), testToken)
	}
}

func TestRateLimitCoolDownEndsAfterRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"number":1,"state":"open"}`))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	gh, err := NewGitHub(Options{BaseURL: srv.URL, Now: clock})
	require.NoError(t, err)
	ctx := context.Background()
	_, err = gh.PRState(ctx, "acme/shop", 1)
	var rl *RateLimitError
	require.True(t, errors.As(err, &rl))
	require.Equal(t, 120*time.Second, rl.RetryAfter)
	mu.Lock()
	now = now.Add(119 * time.Second)
	mu.Unlock()
	_, err = gh.PRState(ctx, "acme/shop", 1)
	require.True(t, errors.As(err, &rl))
	require.Equal(t, time.Second, rl.RetryAfter, "the remaining wait")
	require.Equal(t, int32(1), hits.Load())
	mu.Lock()
	now = now.Add(time.Second)
	mu.Unlock()
	st, err := gh.PRState(ctx, "acme/shop", 1)
	require.NoError(t, err, "the cool-down is over at Retry-After")
	require.Equal(t, PROpen, st.State)
	require.Equal(t, int32(2), hits.Load())
}

func TestRetryAfterParsingAndBounds(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	h := func(kv ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			out.Set(kv[i], kv[i+1])
		}
		return out
	}
	require.Equal(t, 30*time.Second, retryAfter(h("Retry-After", "30"), now))
	require.Equal(t, 45*time.Second, retryAfter(h("Retry-After",
		now.Add(45*time.Second).Format(http.TimeFormat)), now))
	require.Equal(t, time.Minute, retryAfter(h(), now), "no header: one minute")
	require.Equal(t, time.Minute, retryAfter(h("Retry-After", "soon"), now))
	require.Equal(t, time.Second, retryAfter(h("Retry-After", "0"), now), "at least 1s")
	require.Equal(t, time.Second, retryAfter(h("Retry-After", "-5"), now))
	require.Equal(t, time.Hour, retryAfter(h("Retry-After", "86400"), now), "at most 1h")
	require.Equal(t, 10*time.Second, retryAfter(h("X-RateLimit-Reset",
		jsonInt(now.Add(10*time.Second).Unix())), now))
}

func TestConcurrencyCapAndContextWhileWaiting(t *testing.T) {
	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		<-release
		_, _ = w.Write([]byte(`{"number":1,"state":"open"}`))
	}))
	defer srv.Close()
	gh, err := NewGitHub(Options{BaseURL: srv.URL, MaxConcurrent: 2})
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = gh.PRState(context.Background(), "acme/shop", 1)
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for inFlight.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = gh.PRState(ctx, "acme/shop", 1)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "waiting for a request slot")
	close(release)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(2), peak.Load())
}

func TestEgressFloorAppliesToRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()
	gh, err := NewGitHub(Options{BaseURL: srv.URL, Token: testToken})
	require.NoError(t, err)
	_, err = gh.PRState(context.Background(), "acme/shop", 1)
	require.ErrorIs(t, err, egress.ErrBlocked)
	require.False(t, strings.Contains(err.Error(), testToken))
}
