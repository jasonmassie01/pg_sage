package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
)

// ux-01b on a real sage.users: the seen time is NULL on a first visit,
// only moves forward (GREATEST) and stays per user.

func inboxSeenUser(t *testing.T, pool *pgxpool.Pool, email string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.users (email, password)
		VALUES ($1, 'x') RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	return id
}

func TestInboxSeenDB_FirstVisitThenMark(t *testing.T) {
	pool := planDB(t, "inbox_seen_mark")
	store := pgInboxSeen{pool: pool}
	ctx := context.Background()
	alice, bob := inboxSeenUser(t, pool, "a@example.com"), inboxSeenUser(t, pool, "b@example.com")
	if seen, err := store.InboxSeen(ctx, alice); err != nil || seen != nil {
		t.Fatalf("first visit = %v, %v; want NULL", seen, err)
	}
	at := time.Date(2026, 11, 16, 9, 0, 0, 123456000, time.UTC)
	got, err := store.MarkInboxSeen(ctx, alice, at)
	if err != nil || !got.Equal(at) {
		t.Fatalf("mark = %v, %v; want %v", got, err, at)
	}
	seen, err := store.InboxSeen(ctx, alice)
	if err != nil || seen == nil || !seen.Equal(at) {
		t.Fatalf("read back = %v, %v; want %v", seen, err, at)
	}
	if other, err := store.InboxSeen(ctx, bob); err != nil || other != nil {
		t.Fatalf("bob's seen time = %v, %v; want NULL (per user)", other, err)
	}
}

// Boundary and state: an equal time leaves the row as it is, an earlier
// one never moves it back, a later one moves it forward.
func TestInboxSeenDB_OnlyMovesForward(t *testing.T) {
	pool := planDB(t, "inbox_seen_forward")
	store := pgInboxSeen{pool: pool}
	ctx := context.Background()
	id := inboxSeenUser(t, pool, "f@example.com")
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	steps := []struct {
		mark, want time.Time
	}{
		{base, base}, {base, base}, {base.Add(-time.Hour), base},
		{base.Add(time.Second), base.Add(time.Second)},
	}
	for i, s := range steps {
		got, err := store.MarkInboxSeen(ctx, id, s.mark)
		if err != nil || !got.Equal(s.want) {
			t.Fatalf("step %d: mark %v = %v, %v; want %v", i, s.mark, got, err, s.want)
		}
	}
}

func TestInboxSeenDB_UnknownUserIsDistinct(t *testing.T) {
	pool := planDB(t, "inbox_seen_nouser")
	_, err := pgInboxSeen{pool: pool}.MarkInboxSeen(context.Background(), 987654,
		time.Now())
	if !errors.Is(err, errInboxSeenNoUser) {
		t.Fatalf("mark for a missing user = %v, want errInboxSeenNoUser", err)
	}
	seen, err := pgInboxSeen{pool: pool}.InboxSeen(context.Background(), 987654)
	if !errors.Is(err, errInboxSeenNoUser) || seen != nil {
		t.Fatalf("read for a missing user = %v, %v; want errInboxSeenNoUser", seen, err)
	}
}

// Concurrency: two tabs (here twenty) marking at once leave the later time.
func TestInboxSeenDB_ConcurrentMarksKeepTheLaterTime(t *testing.T) {
	pool := planDB(t, "inbox_seen_race")
	store := pgInboxSeen{pool: pool}
	ctx := context.Background()
	id := inboxSeenUser(t, pool, "race@example.com")
	base := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// The latest time goes first, so a plain SET would regress it.
			if _, err := store.MarkInboxSeen(ctx, id,
				base.Add(time.Duration(19-i)*time.Minute)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent mark: %v", err)
	}
	seen, err := store.InboxSeen(ctx, id)
	if err != nil || seen == nil || !seen.Equal(base.Add(19*time.Minute)) {
		t.Fatalf("after the race seen = %v, %v; want %v", seen, err, base.Add(19*time.Minute))
	}
}

// The handlers on the real store: a POST from the page, then a GET from
// another device of the same person.
func TestInboxSeenDB_RoutesRoundTrip(t *testing.T) {
	pool := planDB(t, "inbox_seen_routes")
	id := inboxSeenUser(t, pool, "r@example.com")
	user := &auth.User{ID: id, Email: "r@example.com", Role: auth.RoleOperator}
	store := pgInboxSeen{pool: pool}
	code, out := seenRequest(t, store, "POST", `{"seen_at":"2026-11-16T08:00:00Z"}`, user)
	if code != http.StatusOK || out["seen_at"] != "2026-11-16T08:00:00Z" {
		t.Fatalf("POST: %d %v", code, out)
	}
	code, out = seenRequest(t, store, "GET", "", user)
	if code != http.StatusOK || out["seen_at"] != "2026-11-16T08:00:00Z" {
		t.Fatalf("GET: %d %v, want the time just marked", code, out)
	}
	gone := &auth.User{ID: id + 1000, Role: auth.RoleOperator}
	if code, _ = seenRequest(t, store, "GET", "", gone); code != http.StatusNotFound {
		t.Fatalf("GET for a missing user row answered %d, want 404", code)
	}
}
