package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The Inbox's "Since you were last here" (ux-01b): GET /api/v1/inbox/seen
// answers {seen_at} for the signed-in person (null on a first visit) and
// POST /api/v1/inbox/seen moves it forward, to the body's seen_at (the time
// the page showed, never later than now) or to now. The time lives on
// sage.users.inbox_seen_at, so it follows the person across devices, and
// only moves forward (GREATEST), so two tabs marking at once keep the later
// time. Every role may read and mark its own time; no one else's.

const inboxSeenPath = "/api/v1/inbox/seen"

// errInboxSeenNoUser is a signed-in user whose row is gone.
var errInboxSeenNoUser = errors.New("user not found")

type inboxSeenStore interface {
	InboxSeen(ctx context.Context, userID int) (*time.Time, error)
	MarkInboxSeen(ctx context.Context, userID int, at time.Time) (time.Time, error)
}

// pgInboxSeen keeps the seen time on sage.users of the control database.
type pgInboxSeen struct{ pool *pgxpool.Pool }

func (s pgInboxSeen) InboxSeen(ctx context.Context, userID int) (*time.Time, error) {
	var seen *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT inbox_seen_at FROM sage.users WHERE id = $1`, userID).Scan(&seen)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errInboxSeenNoUser
	}
	if err != nil {
		return nil, fmt.Errorf("read inbox_seen_at of user %d: %w", userID, err)
	}
	return utcPtr(seen), nil
}

func (s pgInboxSeen) MarkInboxSeen(ctx context.Context, userID int,
	at time.Time) (time.Time, error) {
	var seen time.Time
	err := s.pool.QueryRow(ctx, `UPDATE sage.users
		SET inbox_seen_at = GREATEST(inbox_seen_at, $2::timestamptz)
		WHERE id = $1 RETURNING inbox_seen_at`, userID, at).Scan(&seen)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, errInboxSeenNoUser
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("set inbox_seen_at of user %d: %w", userID, err)
	}
	return seen.UTC(), nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// registerInboxSeenRoutes registers both routes on the control pool; with
// no pool there is no user table and the routes are absent (404), which
// the page reads as a first visit.
func registerInboxSeenRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	store := pgInboxSeen{pool: pool}
	route(mux, "GET "+inboxSeenPath, gateViewer, inboxSeenReadHandler(store))
	route(mux, "POST "+inboxSeenPath, gateViewerWrite, inboxSeenMarkHandler(store, time.Now))
}

// seenUserID is the caller's user row id, or 0 after answering 401.
func seenUserID(w http.ResponseWriter, r *http.Request) int {
	user := UserFromContext(r.Context())
	if user == nil || user.ID <= 0 {
		jsonError(w, "the inbox seen time is kept for a signed-in user", http.StatusUnauthorized)
		return 0
	}
	return user.ID
}

func inboxSeenReadHandler(store inboxSeenStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := seenUserID(w, r)
		if id == 0 {
			return
		}
		seen, err := store.InboxSeen(r.Context(), id)
		if seenFailed(w, id, "read", err) {
			return
		}
		jsonResponse(w, map[string]any{"seen_at": seen})
	}
}

func inboxSeenMarkHandler(store inboxSeenStore, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := seenUserID(w, r)
		if id == 0 {
			return
		}
		at, err := decodeSeenAt(r.Body, now().UTC())
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		seen, err := store.MarkInboxSeen(r.Context(), id, at)
		if seenFailed(w, id, "record", err) {
			return
		}
		jsonResponse(w, map[string]any{"seen_at": seen.UTC()})
	}
}

// decodeSeenAt reads an optional {"seen_at": RFC 3339}; absent is now and
// a time after now is now (a client clock never moves it into the future).
func decodeSeenAt(body io.Reader, now time.Time) (time.Time, error) {
	var in struct {
		SeenAt *time.Time `json:"seen_at"`
	}
	if body != nil {
		dec := json.NewDecoder(body)
		err := dec.Decode(&in)
		if err != nil && !errors.Is(err, io.EOF) {
			return time.Time{}, errors.New(
				"body must be JSON {\"seen_at\": an RFC 3339 time} or empty")
		}
		if err == nil && dec.More() {
			return time.Time{}, errors.New("body must hold one JSON object with seen_at")
		}
	}
	if in.SeenAt == nil || in.SeenAt.After(now) {
		return now, nil
	}
	return in.SeenAt.UTC(), nil
}

// seenFailed answers a store error: 404 for a missing user row, else a
// generic 500 that names the step (the pgx text is logged, never sent).
func seenFailed(w http.ResponseWriter, userID int, step string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errInboxSeenNoUser):
		jsonError(w, "user not found", http.StatusNotFound)
	default:
		slog.Error("inbox seen time failed", "step", step, "user_id", userID, "error", err)
		jsonError(w, "could not "+step+" the inbox seen time", http.StatusInternalServerError)
	}
	return true
}
