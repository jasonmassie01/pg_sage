package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
)

// ux-01b: GET and POST /api/v1/inbox/seen keep the time a person last saw
// the Inbox on their own user row, so it follows them across devices.
// These tests drive the handlers over a fake store; inbox_seen_db_test.go
// runs the same handlers on a real sage.users table.

type fakeInboxSeen struct {
	mu      sync.Mutex
	seen    map[int]time.Time
	marks   []time.Time
	readErr error
	markErr error
}

func (f *fakeInboxSeen) InboxSeen(_ context.Context, userID int) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return nil, f.readErr
	}
	at, ok := f.seen[userID]
	if !ok {
		return nil, nil
	}
	return &at, nil
}

func (f *fakeInboxSeen) MarkInboxSeen(_ context.Context, userID int,
	at time.Time) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, at)
	if f.markErr != nil {
		return time.Time{}, f.markErr
	}
	if prev, ok := f.seen[userID]; ok && prev.After(at) {
		return prev, nil
	}
	if f.seen == nil {
		f.seen = map[int]time.Time{}
	}
	f.seen[userID] = at
	return at, nil
}

var inboxSeenNow = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func inboxSeenMux(store inboxSeenStore) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET "+inboxSeenPath, inboxSeenReadHandler(store))
	mux.Handle("POST "+inboxSeenPath, inboxSeenMarkHandler(store,
		func() time.Time { return inboxSeenNow }))
	return mux
}

func seenRequest(t *testing.T, store inboxSeenStore, method, body string,
	user *auth.User) (int, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, inboxSeenPath, nil)
	} else {
		req = httptest.NewRequest(method, inboxSeenPath, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if user != nil {
		req = withUser(req, user)
	}
	w := httptest.NewRecorder()
	inboxSeenMux(store).ServeHTTP(w, req)
	out := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response %q is not JSON: %v", w.Body.String(), err)
	}
	return w.Code, out
}

var seenViewer = &auth.User{ID: 7, Email: "v@example.com", Role: auth.RoleViewer}

func TestInboxSeen_FirstVisitReadsNull(t *testing.T) {
	code, body := seenRequest(t, &fakeInboxSeen{}, "GET", "", seenViewer)
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200: %v", code, body)
	}
	v, present := body["seen_at"]
	if !present || v != nil {
		t.Fatalf("seen_at = %v (present %v), want an explicit null", v, present)
	}
}

func TestInboxSeen_ReadReturnsTheStoredTime(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 30, 15, 0, time.UTC)
	store := &fakeInboxSeen{seen: map[int]time.Time{7: at, 8: inboxSeenNow}}
	_, body := seenRequest(t, store, "GET", "", seenViewer)
	if body["seen_at"] != "2026-11-10T09:30:15Z" {
		t.Fatalf("seen_at = %v, want the viewer's own time 2026-11-10T09:30:15Z", body["seen_at"])
	}
}

func TestInboxSeen_MarkWithoutATimeUsesTheServerClock(t *testing.T) {
	for _, body := range []string{"", "{}"} {
		store := &fakeInboxSeen{}
		code, out := seenRequest(t, store, "POST", body, seenViewer)
		if code != http.StatusOK || out["seen_at"] != "2026-11-16T12:00:00Z" {
			t.Fatalf("body %q: %d %v, want 200 with the server's now", body, code, out)
		}
		if len(store.marks) != 1 || !store.marks[0].Equal(inboxSeenNow) {
			t.Fatalf("body %q: marks = %v, want one at now", body, store.marks)
		}
	}
}

func TestInboxSeen_MarkKeepsAPastTimeAndClampsAFutureOne(t *testing.T) {
	past := "2026-11-16T11:59:55+02:00" // 09:59:55Z
	store := &fakeInboxSeen{}
	_, out := seenRequest(t, store, "POST", `{"seen_at":"`+past+`"}`, seenViewer)
	if out["seen_at"] != "2026-11-16T09:59:55Z" {
		t.Fatalf("seen_at = %v, want the page's time in UTC", out["seen_at"])
	}
	future := &fakeInboxSeen{}
	_, out = seenRequest(t, future, "POST", `{"seen_at":"2026-11-17T00:00:00Z"}`, seenViewer)
	if out["seen_at"] != "2026-11-16T12:00:00Z" || !future.marks[0].Equal(inboxSeenNow) {
		t.Fatalf("a future time stored %v (%v), want it clamped to now", out["seen_at"],
			future.marks)
	}
}

func TestInboxSeen_LaterMarkWinsAndEarlierDoesNotRegress(t *testing.T) {
	late := time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)
	store := &fakeInboxSeen{seen: map[int]time.Time{7: late}}
	_, out := seenRequest(t, store, "POST", `{"seen_at":"2026-11-16T10:00:00Z"}`, seenViewer)
	if out["seen_at"] != "2026-11-16T11:00:00Z" {
		t.Fatalf("seen_at = %v, want the later stored time kept", out["seen_at"])
	}
}

func TestInboxSeen_RejectsBadInput(t *testing.T) {
	for name, body := range map[string]string{
		"not JSON":      `{seen_at`,
		"not RFC 3339":  `{"seen_at":"yesterday"}`,
		"a number":      `{"seen_at":12}`,
		"trailing data": `{} {}`,
	} {
		store := &fakeInboxSeen{}
		code, out := seenRequest(t, store, "POST", body, seenViewer)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, code)
		}
		if msg, _ := out["error"].(string); !strings.Contains(msg, "seen_at") {
			t.Errorf("%s: error %q does not name seen_at", name, msg)
		}
		if len(store.marks) != 0 {
			t.Errorf("%s: a rejected body was stored: %v", name, store.marks)
		}
	}
}

func TestInboxSeen_NeedsASignedInUser(t *testing.T) {
	for _, method := range []string{"GET", "POST"} {
		code, out := seenRequest(t, &fakeInboxSeen{}, method, "", nil)
		if code != http.StatusUnauthorized {
			t.Errorf("%s with no user: %d %v, want 401", method, code, out)
		}
		code, _ = seenRequest(t, &fakeInboxSeen{}, method, "",
			&auth.User{ID: 0, Role: auth.RoleAdmin})
		if code != http.StatusUnauthorized {
			t.Errorf("%s with user id 0: %d, want 401 (no user row to keep it on)", method, code)
		}
	}
}

func TestInboxSeen_StoreErrorsAreGenericAndDistinct(t *testing.T) {
	leak := errors.New("dial tcp 10.0.0.9:5432: connection refused")
	code, out := seenRequest(t, &fakeInboxSeen{readErr: leak}, "GET", "", seenViewer)
	msg, _ := out["error"].(string)
	if code != http.StatusInternalServerError || strings.Contains(msg, "10.0.0.9") ||
		!strings.Contains(msg, "read") {
		t.Fatalf("read failure: %d %q, want 500 naming the read, no pgx text", code, msg)
	}
	code, out = seenRequest(t, &fakeInboxSeen{markErr: leak}, "POST", "{}", seenViewer)
	msg, _ = out["error"].(string)
	if code != http.StatusInternalServerError || strings.Contains(msg, "10.0.0.9") ||
		!strings.Contains(msg, "record") {
		t.Fatalf("mark failure: %d %q, want 500 naming the write, no pgx text", code, msg)
	}
	code, out = seenRequest(t, &fakeInboxSeen{markErr: errInboxSeenNoUser}, "POST", "{}",
		seenViewer)
	if code != http.StatusNotFound {
		t.Fatalf("a deleted user row: %d %v, want 404", code, out)
	}
}

func TestInboxSeen_NoControlPoolRegistersNothing(t *testing.T) {
	mux := http.NewServeMux()
	registerInboxSeenRoutes(mux, nil)
	req := withUser(httptest.NewRequest("GET", inboxSeenPath, nil), seenViewer)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no pool: GET answered %d, want 404 (the page treats it as a first visit)",
			w.Code)
	}
}

func TestInboxSeen_ConcurrentMarksKeepTheLatest(t *testing.T) {
	store := &fakeInboxSeen{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			at := inboxSeenNow.Add(-time.Duration(20-i) * time.Minute).Format(time.RFC3339)
			seenRequest(t, store, "POST", `{"seen_at":"`+at+`"}`, seenViewer)
		}(i)
	}
	wg.Wait()
	_, out := seenRequest(t, store, "GET", "", seenViewer)
	if out["seen_at"] != "2026-11-16T11:59:00Z" {
		t.Fatalf("seen_at = %v after 20 marks, want the latest 11:59:00Z", out["seen_at"])
	}
}
