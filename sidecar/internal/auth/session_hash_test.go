package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// apiauth-04: sage.sessions stores only the SHA-256 of the cookie, so a
// reader of the sage schema (pg_read_all_data, a dump, a restored backup)
// cannot lift a live session.

func newSessionFixture(t *testing.T, email string) (*pgxpool.Pool, int) {
	t.Helper()
	pool := setupOIDCPool(t)
	return pool, linkTestUser(t, pool, email, RoleAdmin)
}

func TestSessionCookieIsNeverStored(t *testing.T) {
	pool, uid := newSessionFixture(t, "hash-store@example.com")
	ctx := context.Background()
	tok, err := CreateSession(ctx, pool, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	var leaks int
	// Any column, in any text form, containing the cookie is a leak.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.sessions s
		WHERE strpos(to_jsonb(s)::text, $1) > 0 OR s.id::text = $1 OR s.token_hash = $1`,
		tok).Scan(&leaks); err != nil {
		t.Fatalf("scanning for the cookie: %v", err)
	}
	if leaks != 0 {
		t.Fatalf("%d session rows contain the cookie value", leaks)
	}
	var stored string
	var owner int
	if err := pool.QueryRow(ctx, `SELECT token_hash, user_id FROM sage.sessions
		WHERE token_hash = $1`, HashSessionToken(tok)).Scan(&stored, &owner); err != nil {
		t.Fatalf("hashed row missing: %v", err)
	}
	if owner != uid || !hexSHA256.MatchString(stored) {
		t.Fatalf("row = (%q, %d), want (sha256 hex, %d)", stored, owner, uid)
	}
}

func TestValidateSession_ByHash(t *testing.T) {
	pool, uid := newSessionFixture(t, "hash-validate@example.com")
	ctx := context.Background()
	tok, err := CreateSession(ctx, pool, uid)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	u, err := ValidateSession(ctx, pool, tok)
	if err != nil || u == nil || u.ID != uid || u.Role != RoleAdmin {
		t.Fatalf("ValidateSession(cookie) = (%+v, %v), want user %d", u, err, uid)
	}
	var rowID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM sage.sessions
		WHERE token_hash = $1`, HashSessionToken(tok)).Scan(&rowID); err != nil {
		t.Fatalf("reading row id: %v", err)
	}
	// What a reader of the table can see must not authenticate.
	for name, stolen := range map[string]string{
		"stored hash": HashSessionToken(tok), "row id": rowID,
	} {
		u, err := ValidateSession(ctx, pool, stolen)
		if u != nil || !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("ValidateSession(%s) = (%v, %v), want ErrSessionInvalid", name, u, err)
		}
	}
}

func TestValidateSession_ExpiredHashedSession(t *testing.T) {
	pool, uid := newSessionFixture(t, "hash-expired@example.com")
	ctx := context.Background()
	tok, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.sessions (user_id, expires_at, token_hash)
		VALUES ($1, now() - interval '1 second', $2)`, uid, HashSessionToken(tok)); err != nil {
		t.Fatalf("inserting expired session: %v", err)
	}
	if u, err := ValidateSession(ctx, pool, tok); u != nil || !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("expired session = (%v, %v), want ErrSessionInvalid", u, err)
	}
}

func TestDeleteSession_ByHashRemovesOnlyThatSession(t *testing.T) {
	pool, uid := newSessionFixture(t, "hash-logout@example.com")
	ctx := context.Background()
	a, err := CreateSession(ctx, pool, uid)
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateSession(ctx, pool, uid)
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteSession(ctx, pool, a); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := ValidateSession(ctx, pool, a); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("deleted session still validates: %v", err)
	}
	if u, err := ValidateSession(ctx, pool, b); err != nil || u.ID != uid {
		t.Fatalf("other session = (%v, %v), want it intact", u, err)
	}
}

// State transitions: sign out everywhere else keeps the caller's session,
// ends the user's other sessions and leaves other users alone; ending all
// then ends the caller's too.
func TestEndUserSessions_SignOutEverywhereElse(t *testing.T) {
	pool, uid := newSessionFixture(t, "hash-everywhere@example.com")
	other := linkTestUser(t, pool, "hash-bystander@example.com", RoleViewer)
	ctx := context.Background()
	mint := func(id int) string {
		tok, err := CreateSession(ctx, pool, id)
		if err != nil {
			t.Fatalf("CreateSession(%d): %v", id, err)
		}
		return tok
	}
	a, b, c, bystander := mint(uid), mint(uid), mint(uid), mint(other)

	n, err := EndUserSessions(ctx, pool, uid, HashSessionToken(a))
	if err != nil || n != 2 {
		t.Fatalf("EndUserSessions(except A) = (%d, %v), want (2, nil)", n, err)
	}
	if u, err := ValidateSession(ctx, pool, a); err != nil || u.ID != uid {
		t.Fatalf("session A = (%v, %v), want it kept", u, err)
	}
	for name, tok := range map[string]string{"B": b, "C": c} {
		if _, err := ValidateSession(ctx, pool, tok); !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("session %s survived: %v", name, err)
		}
	}
	if u, err := ValidateSession(ctx, pool, bystander); err != nil || u.ID != other {
		t.Fatalf("another user's session = (%v, %v), want it intact", u, err)
	}
	if n, err := EndUserSessions(ctx, pool, uid, HashSessionToken(a)); err != nil || n != 0 {
		t.Fatalf("second sign-out = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := EndUserSessions(ctx, pool, uid, ""); err != nil || n != 1 {
		t.Fatalf("EndUserSessions(all) = (%d, %v), want (1, nil)", n, err)
	}
	if _, err := ValidateSession(ctx, pool, a); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("session A survived ending all: %v", err)
	}
}

func TestValidateSession_ConcurrentValidations(t *testing.T) {
	pool, uid := newSessionFixture(t, "hash-parallel@example.com")
	ctx := context.Background()
	tok, err := CreateSession(ctx, pool, uid)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 50
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := ValidateSession(ctx, pool, tok)
			if err == nil && u.ID != uid {
				err = errors.New("validated as the wrong user")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel validation failed: %v", err)
		}
	}
}

func TestCreateSessionWithDuration_StoresHashWithLifetime(t *testing.T) {
	pool, uid := newSessionFixture(t, "hash-lifetime@example.com")
	ctx := context.Background()
	tok, err := CreateSessionWithDuration(ctx, pool, uid, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var secs float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM expires_at - now())
		FROM sage.sessions WHERE token_hash = $1`, HashSessionToken(tok)).Scan(&secs); err != nil {
		t.Fatalf("reading lifetime: %v", err)
	}
	if secs < 55 || secs > 60 {
		t.Fatalf("session lifetime %.1fs, want about 60s", secs)
	}
}
