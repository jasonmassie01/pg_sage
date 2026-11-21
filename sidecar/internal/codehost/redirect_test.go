package codehost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrency tests: one request per client, the redirect check is a
// pure function of the two URLs.

// tokenSink is a second origin that counts the requests carrying the token.
func tokenSink(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var hits, leaked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("PRIVATE-TOKEN") == testToken ||
			r.Header.Get("Authorization") == "Bearer "+testToken {
			leaked.Add(1)
		}
		_, _ = w.Write([]byte(`{"number":1,"state":"open","iid":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &leaked
}

// redirector sends every request to target, keeping the path.
func redirector(t *testing.T, target string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRedirectToAnotherOriginIsRefusedAndTheTokenStaysHome(t *testing.T) {
	sink, hits, leaked := tokenSink(t)
	srv := redirector(t, sink.URL)
	gh, err := NewGitHub(Options{BaseURL: srv.URL, Token: testToken})
	require.NoError(t, err)
	gl, err := NewGitLab(Options{BaseURL: srv.URL, Token: testToken})
	require.NoError(t, err)
	ctx := context.Background()
	_, ghErr := gh.PRState(ctx, "acme/shop", 1)
	_, glErr := gl.PRState(ctx, "acme/shop", 1)
	for _, err := range []error{ghErr, glErr} {
		require.ErrorIs(t, err, ErrUnavailable)
		require.ErrorContains(t, err, "redirect to another origin")
		require.NotContains(t, err.Error(), testToken)
	}
	require.Equal(t, int32(0), hits.Load(), "the other origin is never asked")
	require.Equal(t, int32(0), leaked.Load())
}

func TestRedirectWithinTheSameOriginIsFollowed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/shop/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repositories/42/pulls/1", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/repositories/42/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":1,"state":"closed"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gh, err := NewGitHub(Options{BaseURL: srv.URL, Token: testToken})
	require.NoError(t, err)
	st, err := gh.PRState(context.Background(), "acme/shop", 1)
	require.NoError(t, err)
	require.Equal(t, PRClosed, st.State, "a renamed repository's redirect is followed")
}
