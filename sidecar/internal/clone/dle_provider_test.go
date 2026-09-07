package clone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testDLEToken = "dle-test-token-never-log"

func newDLEProviderForTest(t *testing.T, server *httptest.Server) *DLEProvider {
	t.Helper()
	provider, err := NewDLEProvider(DLEConfig{
		Endpoint:   server.URL,
		Token:      testDLEToken,
		HTTPClient: server.Client(),
		Now:        func() time.Time { return testNow() },
	})
	if err != nil {
		t.Fatalf("NewDLEProvider: %v", err)
	}
	return provider
}

func testNow() time.Time {
	return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func requireBearerAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+testDLEToken {
		t.Fatalf("authorization = %q, want configured bearer token", got)
	}
}

func requireSecretRedacted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testDLEToken) {
		t.Fatalf("error leaked DLE token: %v", err)
	}
}

func TestNewDLEProviderValidatesRequiredConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  DLEConfig
	}{
		{name: "missing endpoint", cfg: DLEConfig{Token: testDLEToken}},
		{name: "missing token", cfg: DLEConfig{Endpoint: "https://dle.invalid"}},
		{name: "invalid endpoint", cfg: DLEConfig{
			Endpoint: "://bad", Token: testDLEToken,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := NewDLEProvider(test.cfg)
			if err == nil || provider != nil {
				t.Fatalf("provider=%v err=%v, want validation failure", provider, err)
			}
			requireSecretRedacted(t, err)
		})
	}
}

func TestDLECreateSendsSpecAndReturnsClone(t *testing.T) {
	createdFrom := testNow().Add(-2 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/clones" {
			t.Fatalf("request = %s %s, want POST /clones", r.Method, r.URL.Path)
		}
		requireBearerAuth(t, r)
		var spec CloneSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			t.Fatalf("decode clone spec: %v", err)
		}
		if !spec.IncludeData || spec.TargetSizeHintBytes != 4<<30 {
			t.Fatalf("clone spec = %#v", spec)
		}
		writeJSON(t, w, http.StatusCreated, Clone{
			ID: "clone-1", DSN: "postgres://clone/app", CreatedFrom: createdFrom,
		})
	}))
	defer server.Close()

	got, err := newDLEProviderForTest(t, server).Create(
		context.Background(), CloneSpec{IncludeData: true, TargetSizeHintBytes: 4 << 30},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID != "clone-1" || got.DSN != "postgres://clone/app" {
		t.Fatalf("clone = %#v", got)
	}
	if !got.CreatedFrom.Equal(createdFrom) {
		t.Fatalf("created-from = %s, want %s", got.CreatedFrom, createdFrom)
	}
}

func TestDLEDestroyUsesEscapedCloneID(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		requireBearerAuth(t, r)
		if r.Method != http.MethodDelete || r.URL.EscapedPath() != "/clones/a%2Fb" {
			t.Fatalf("request = %s %s", r.Method, r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := newDLEProviderForTest(t, server).Destroy(
		context.Background(), Clone{ID: "a/b"},
	)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("destroy requests = %d, want 1", requests.Load())
	}
}

func TestDLEDestroyRejectsMissingCloneIDWithoutRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	err := newDLEProviderForTest(t, server).Destroy(context.Background(), Clone{})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "id") {
		t.Fatalf("Destroy error = %v, want missing ID", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("destroy made %d requests for empty ID", requests.Load())
	}
}

func TestDLESnapshotAgeUsesLatestSourceTimestamp(t *testing.T) {
	sourceTime := testNow().Add(-6*time.Hour - 30*time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearerAuth(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/snapshots/latest" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"created_at": sourceTime})
	}))
	defer server.Close()

	age, err := newDLEProviderForTest(t, server).SnapshotAge(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAge: %v", err)
	}
	if age != 6*time.Hour+30*time.Minute {
		t.Fatalf("snapshot age = %s, want 6h30m", age)
	}
}

func TestDLESnapshotAgeRejectsFutureTimestamp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{
			"created_at": testNow().Add(time.Minute),
		})
	}))
	defer server.Close()

	age, err := newDLEProviderForTest(t, server).SnapshotAge(context.Background())
	if err == nil || age != 0 {
		t.Fatalf("SnapshotAge = %s, %v; want zero and error", age, err)
	}
}

func TestDLEProviderMapsHTTPFailuresWithoutLeakingAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "authorization=Bearer "+testDLEToken, http.StatusUnauthorized)
	}))
	defer server.Close()
	provider := newDLEProviderForTest(t, server)

	_, createErr := provider.Create(context.Background(), CloneSpec{IncludeData: true})
	requireSecretRedacted(t, createErr)
	requireSecretRedacted(t, provider.Destroy(context.Background(), Clone{ID: "clone-1"}))
	_, ageErr := provider.SnapshotAge(context.Background())
	requireSecretRedacted(t, ageErr)
}

func TestDLECreateRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":`))
	}))
	defer server.Close()

	clone, err := newDLEProviderForTest(t, server).Create(
		context.Background(), CloneSpec{IncludeData: true},
	)
	if err == nil || clone.ID != "" {
		t.Fatalf("Create = %#v, %v; want zero clone and decode error", clone, err)
	}
	requireSecretRedacted(t, err)
}

func TestDLESnapshotAgeRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]string{"created_at": "not-a-time"})
	}))
	defer server.Close()

	age, err := newDLEProviderForTest(t, server).SnapshotAge(context.Background())
	if err == nil || age != 0 {
		t.Fatalf("SnapshotAge = %s, %v; want zero and decode error", age, err)
	}
}

func TestDLERequestsHonorCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Windows net/http can return the client cancellation before the test server
		// observes its connection closing. Bound the harness wait so Close cannot hang.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	provider := newDLEProviderForTest(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := provider.Create(ctx, CloneSpec{IncludeData: true})
		done <- err
	}()
	<-started
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Create error = %v, want context canceled", err)
	}
}

func TestDLEProviderSupportsConcurrentCreates(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requireBearerAuth(t, r)
		id := requests.Add(1)
		writeJSON(t, w, http.StatusCreated, Clone{
			ID:          fmt.Sprintf("clone-%d", id),
			DSN:         fmt.Sprintf("postgres://clone-%d/app", id),
			CreatedFrom: testNow().Add(-time.Hour),
		})
	}))
	defer server.Close()
	provider := newDLEProviderForTest(t, server)

	const count = 8
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			clone, err := provider.Create(context.Background(), CloneSpec{IncludeData: true})
			if err == nil && (clone.ID == "" || clone.DSN == "") {
				err = fmt.Errorf("incomplete clone: %#v", clone)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Create: %v", err)
		}
	}
	if requests.Load() != count {
		t.Fatalf("provider requests = %d, want %d", requests.Load(), count)
	}
}

func TestDLECreateCleansUpPartialClone(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/clones":
			writeJSON(t, w, http.StatusCreated, map[string]any{
				"id": "partial-1", "created_from": testNow().Add(-time.Hour),
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/clones/partial-1":
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	clone, err := newDLEProviderForTest(t, server).Create(
		context.Background(), CloneSpec{IncludeData: true},
	)
	if err == nil || clone.ID != "" {
		t.Fatalf("Create = %#v, %v; want validation failure", clone, err)
	}
	if deletes.Load() != 1 {
		t.Fatalf("partial clone deletes = %d, want 1", deletes.Load())
	}
}

func TestDLECreateReportsCleanupFailureWithoutSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writeJSON(t, w, http.StatusCreated, map[string]any{"id": "partial-2"})
			return
		}
		http.Error(w, "cleanup failed "+testDLEToken, http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := newDLEProviderForTest(t, server).Create(
		context.Background(), CloneSpec{IncludeData: true},
	)
	requireSecretRedacted(t, err)
	for _, want := range []string{"invalid clone", "cleanup"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Fatalf("error = %v, want context %q", err, want)
		}
	}
}

func TestDLEDestroyPropagatesCallerCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request should not be sent for canceled context")
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := newDLEProviderForTest(t, server).Destroy(ctx, Clone{ID: "clone-1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Destroy error = %v, want context canceled", err)
	}
}

func TestDLEErrorIncludesOperationAndStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary outage", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := newDLEProviderForTest(t, server).Create(context.Background(), CloneSpec{})
	if err == nil {
		t.Fatal("expected create error")
	}
	for _, want := range []string{"create", fmt.Sprint(http.StatusServiceUnavailable)} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Fatalf("error = %v, want %q", err, want)
		}
	}
}
