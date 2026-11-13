package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// apiauth-13: a session's expiry is computed on the database clock, the
// same clock every lookup (expires_at > now()) and the cleanup
// (expires_at <= now()) judge it by. created_at defaults to now() in the
// same INSERT, so expires_at - created_at is exactly the lifetime only
// when both come from the database clock; a host-clock expiry is off by
// the skew plus the round trip.

// storedLifetime reads expires_at - created_at for one session, in seconds.
func storedLifetime(t *testing.T, pool *pgxpool.Pool, tok string) float64 {
	t.Helper()
	var secs float64
	if err := pool.QueryRow(context.Background(), `SELECT
		extract(epoch FROM expires_at - created_at)::float8
		FROM sage.sessions WHERE token_hash = $1`,
		HashSessionToken(tok)).Scan(&secs); err != nil {
		t.Fatalf("reading stored lifetime: %v", err)
	}
	return secs
}

func countUserSessions(t *testing.T, pool *pgxpool.Pool, uid int) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sage.sessions WHERE user_id = $1`, uid).Scan(&n); err != nil {
		t.Fatalf("counting sessions: %v", err)
	}
	return n
}

func TestCreateSessionWithDuration_ExpiryOnDatabaseClock(t *testing.T) {
	pool, uid := newSessionFixture(t, "clock-happy@example.com")
	ctx := context.Background()
	d := time.Hour
	tok, err := CreateSessionWithDuration(ctx, pool, uid, d)
	if err != nil {
		t.Fatalf("CreateSessionWithDuration: %v", err)
	}
	if got := storedLifetime(t, pool, tok); got != d.Seconds() {
		t.Fatalf("expires_at - created_at = %.6fs, want exactly %.0fs "+
			"(expiry not on the database clock)", got, d.Seconds())
	}
	var left float64
	if err := pool.QueryRow(ctx, `SELECT extract(epoch FROM expires_at - now())::float8
		FROM sage.sessions WHERE token_hash = $1`, HashSessionToken(tok)).Scan(&left); err != nil {
		t.Fatalf("reading time left: %v", err)
	}
	if left > d.Seconds() || left <= d.Seconds()-2 {
		t.Fatalf("time left %.6fs, want in (%.0fs, %.0fs]", left, d.Seconds()-2, d.Seconds())
	}
}

func TestCreateSessionWithDuration_NonPositiveRefusedWithoutRow(t *testing.T) {
	pool, uid := newSessionFixture(t, "clock-invalid@example.com")
	for _, d := range []time.Duration{0, -time.Second, -time.Nanosecond} {
		tok, err := CreateSessionWithDuration(context.Background(), pool, uid, d)
		if err == nil || !strings.Contains(err.Error(), "duration must be positive") {
			t.Fatalf("d=%v: err = %v, want \"duration must be positive\"", d, err)
		}
		if tok != "" {
			t.Fatalf("d=%v: returned token %q with the error", d, tok)
		}
	}
	if n := countUserSessions(t, pool, uid); n != 0 {
		t.Fatalf("%d session rows after refused creations, want 0", n)
	}
}

func TestCreateSessionWithDuration_NilPoolRefused(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil pool panicked: %v", r)
		}
	}()
	tok, err := CreateSessionWithDuration(context.Background(), nil, 1, time.Hour)
	if err == nil || err.Error() != "creating session: no database pool" {
		t.Fatalf("nil pool err = %v, want \"creating session: no database pool\"", err)
	}
	if tok != "" {
		t.Fatalf("nil pool returned token %q", tok)
	}
	// The duration is checked first, so a bad duration names itself.
	_, err = CreateSessionWithDuration(context.Background(), nil, 1, 0)
	if err == nil || !strings.Contains(err.Error(), "duration must be positive") {
		t.Fatalf("nil pool, zero duration: err = %v, want the duration refusal", err)
	}
}

func TestCreateSessionWithDuration_MissingTableIsWrapped(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "nosessions")
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool on the empty database: %v", err)
	}
	t.Cleanup(pool.Close)
	tok, err := CreateSessionWithDuration(context.Background(), pool, 1, time.Hour)
	if err == nil || tok != "" {
		t.Fatalf("= (%q, %v), want an error and no token", tok, err)
	}
	if !strings.HasPrefix(err.Error(), "creating session: ") {
		t.Fatalf("error %q does not name the step", err)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42P01" {
		t.Fatalf("error %v does not wrap SQLSTATE 42P01 (undefined_table)", err)
	}
}

func TestCreateSessionWithDuration_MicrosecondBoundary(t *testing.T) {
	pool, uid := newSessionFixture(t, "clock-boundary@example.com")
	ctx := context.Background()
	cases := []struct {
		d     time.Duration
		want  float64
		valid bool
	}{
		{time.Nanosecond, 0, false},         // below the stored precision
		{time.Microsecond, 0.000001, false}, // expires before any lookup
		{time.Second, 1, true},
	}
	for _, c := range cases {
		tok, err := CreateSessionWithDuration(ctx, pool, uid, c.d)
		if err != nil {
			t.Fatalf("d=%v: %v", c.d, err)
		}
		if got := storedLifetime(t, pool, tok); got != c.want {
			t.Fatalf("d=%v: stored lifetime %.7fs, want exactly %.7fs", c.d, got, c.want)
		}
		u, err := ValidateSession(ctx, pool, tok)
		if c.valid && (err != nil || u == nil || u.ID != uid) {
			t.Fatalf("d=%v: ValidateSession = (%v, %v), want user %d", c.d, u, err, uid)
		}
		if !c.valid && (!errors.Is(err, ErrSessionInvalid) || u != nil) {
			t.Fatalf("d=%v: ValidateSession = (%v, %v), want ErrSessionInvalid", c.d, u, err)
		}
	}
	// The lookup's predicate (expires_at > now()) evaluated at database
	// times measured from the creating statement: a 1 s session is valid
	// 0.9 s after creation and expired 1 s after. Measuring time left with
	// a second query would test the round trip, not the stored expiry.
	tok, err := CreateSessionWithDuration(ctx, pool, uid, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var validAt09, validAt1 bool
	if err := pool.QueryRow(ctx, `SELECT expires_at > created_at + interval '0.9 second',
		expires_at > created_at + interval '1 second'
		FROM sage.sessions WHERE token_hash = $1`, HashSessionToken(tok),
	).Scan(&validAt09, &validAt1); err != nil {
		t.Fatal(err)
	}
	if !validAt09 || validAt1 {
		t.Fatalf("1s session valid at +0.9s=%v, at +1s=%v; want true, false",
			validAt09, validAt1)
	}
}

func TestCreateSessionWithDuration_ConcurrentForOneUser(t *testing.T) {
	pool, uid := newSessionFixture(t, "clock-concurrent@example.com")
	const n = 20
	tokens := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = CreateSessionWithDuration(
				context.Background(), pool, uid, time.Hour)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	var rows, distinct, exact int
	if err := pool.QueryRow(context.Background(), `SELECT count(*),
		count(DISTINCT token_hash),
		count(*) FILTER (WHERE expires_at - created_at = interval '1 hour')
		FROM sage.sessions WHERE user_id = $1`, uid).Scan(&rows, &distinct, &exact); err != nil {
		t.Fatalf("reading sessions: %v", err)
	}
	if rows != n || distinct != n || exact != n {
		t.Fatalf("rows=%d distinct=%d exact-lifetime=%d, want %d each", rows, distinct, exact, n)
	}
	for i, tok := range tokens {
		if u, err := ValidateSession(context.Background(), pool, tok); err != nil || u.ID != uid {
			t.Fatalf("token %d: ValidateSession = (%v, %v)", i, u, err)
		}
	}
}

func TestCreateSessionWithDuration_ExpiresThenCleaned(t *testing.T) {
	pool, uid := newSessionFixture(t, "clock-states@example.com")
	ctx := context.Background()
	tok, err := CreateSessionWithDuration(ctx, pool, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := CreateSessionWithDuration(ctx, pool, uid, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := ValidateSession(ctx, pool, tok); err != nil || u.ID != uid {
		t.Fatalf("fresh session: ValidateSession = (%v, %v)", u, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.sessions
		SET expires_at = now() - interval '1 second' WHERE token_hash = $1`,
		HashSessionToken(tok)); err != nil {
		t.Fatalf("expiring the session: %v", err)
	}
	if u, err := ValidateSession(ctx, pool, tok); !errors.Is(err, ErrSessionInvalid) || u != nil {
		t.Fatalf("expired session: ValidateSession = (%v, %v), want ErrSessionInvalid", u, err)
	}
	if err := CleanExpiredSessions(ctx, pool); err != nil {
		t.Fatalf("CleanExpiredSessions: %v", err)
	}
	var gone, kept int
	if err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE token_hash = $1), count(*) FILTER (WHERE token_hash = $2)
		FROM sage.sessions`, HashSessionToken(tok), HashSessionToken(keep),
	).Scan(&gone, &kept); err != nil {
		t.Fatal(err)
	}
	if gone != 0 || kept != 1 {
		t.Fatalf("after cleanup: expired rows=%d live rows=%d, want 0 and 1", gone, kept)
	}
}
