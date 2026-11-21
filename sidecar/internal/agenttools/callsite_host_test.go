package agenttools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The call-site finder over the real GitHub client against a recorded
// host, for the failures only a REST client produces.

const csToken = "ghp_callsiteSECRETtoken0123456789"

func githubClient(t *testing.T, srv *httptest.Server, timeout time.Duration) *codehost.GitHub {
	t.Helper()
	gh, err := codehost.NewGitHub(codehost.Options{BaseURL: srv.URL, Token: csToken,
		Timeout: timeout})
	require.NoError(t, err)
	return gh
}

func contentsAnswer(content string) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "file", "encoding": "base64",
		"size": len(content), "sha": "abc",
		"content": base64.StdEncoding.EncodeToString([]byte(content))})
	return raw
}

func TestFindCallSiteGitHub403NamesTheRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()
	_, err := FindCallSite(context.Background(), githubClient(t, srv, 0), csRepo, 3,
		fileLineTags("app/models/order.rb", "42"), "")
	require.ErrorIs(t, err, codehost.ErrForbidden)
	require.ErrorContains(t, err, "token cannot read acme/shop")
	require.NotContains(t, err.Error(), csToken)
}

func TestFindCallSiteGitHub429HonoursRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	gh := githubClient(t, srv, 0)
	tags := fileLineTags("app/models/order.rb", "42")
	_, err := FindCallSite(context.Background(), gh, csRepo, 3, tags, "")
	var rl *codehost.RateLimitError
	require.True(t, errors.As(err, &rl), "want a rate-limit error, got %v", err)
	require.Equal(t, 30*time.Second, rl.RetryAfter)
	require.ErrorIs(t, err, codehost.ErrRateLimited)
	require.False(t, errors.Is(err, codehost.ErrForbidden), "distinguishable from a 403")
	_, err = FindCallSite(context.Background(), gh, csRepo, 3, tags, "")
	require.ErrorIs(t, err, codehost.ErrRateLimited)
	require.Equal(t, int32(1), hits.Load(), "nothing is sent during the cool-down")
}

func TestFindCallSiteGitHubTimeoutNamesTheHost(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	_, err := FindCallSite(context.Background(), githubClient(t, srv, 50*time.Millisecond),
		csRepo, 3, fileLineTags("app/models/order.rb", "42"), "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	host := strings.TrimPrefix(srv.URL, "http://")
	require.ErrorContains(t, err, "github "+host)
}

func TestFindCallSiteParallelLookupsRespectHostCap(t *testing.T) {
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write(contentsAnswer(rubyLines(50)))
	}))
	defer srv.Close()
	gh := githubClient(t, srv, 0)
	var wg sync.WaitGroup
	results := make([]CallSite, 20)
	errs := make([]error, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = FindCallSite(context.Background(), gh, csRepo,
				QueryID(i+1), fileLineTags("app/models/order.rb", "42"), "")
		}(i)
	}
	wg.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		require.Equal(t, CallSiteExact, results[i].Confidence)
		require.Equal(t, QueryID(i+1), results[i].QueryID)
	}
	require.LessOrEqual(t, peak.Load(), int32(4), "the per-host cap is 4")
	require.Greater(t, peak.Load(), int32(1), "lookups did run in parallel")
}
