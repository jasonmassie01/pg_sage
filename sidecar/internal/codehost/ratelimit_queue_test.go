package codehost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// A request already waiting for a slot when a rate-limit answer arrives
// is refused, not sent: the cool-down covers queued requests too.
func TestRequestQueuedForASlotIsNotSentAfterARateLimit(t *testing.T) {
	var hits atomic.Int32
	first := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			close(first)
			<-release
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	gh, err := NewGitHub(Options{BaseURL: srv.URL, MaxConcurrent: 1})
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	call := func(i int) {
		defer wg.Done()
		_, errs[i] = gh.PRState(context.Background(), "acme/shop", 1)
	}
	wg.Add(1)
	go call(0)
	<-first
	wg.Add(1)
	go call(1)
	// Let the second request pass the cool-down check and wait for the slot.
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, err := range errs {
		var rl *RateLimitError
		require.True(t, errors.As(err, &rl), "request %d: %v", i, err)
	}
	require.Equal(t, int32(1), hits.Load(), "the queued request reached the host")
}
