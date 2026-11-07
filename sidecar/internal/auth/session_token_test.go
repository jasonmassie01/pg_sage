package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// No concurrent access tests here: the token helpers are pure functions;
// parallel validation against a real session is in session_hash_test.go.

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestNewSessionToken_Shape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := newSessionToken()
		if err != nil {
			t.Fatalf("newSessionToken: %v", err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token %q is not base64url: %v", tok, err)
		}
		if len(raw) != 32 {
			t.Fatalf("token decodes to %d bytes, want 32", len(raw))
		}
		if seen[tok] {
			t.Fatalf("token %q repeated", tok)
		}
		seen[tok] = true
		if _, ok := sessionTokenHash(tok); !ok {
			t.Fatalf("freshly minted token %q rejected by the format check", tok)
		}
	}
}

func TestHashSessionToken_IsSHA256Hex(t *testing.T) {
	tok, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	h := HashSessionToken(tok)
	if !hexSHA256.MatchString(h) {
		t.Fatalf("hash %q is not 64 lowercase hex chars", h)
	}
	if h == tok || strings.Contains(h, tok) {
		t.Fatal("hash contains the token")
	}
	if HashSessionToken(tok) != h {
		t.Fatal("hash is not deterministic")
	}
	// The link-grant and session hashes share one helper.
	if hashToken(tok) != h {
		t.Fatalf("hashToken = %q, HashSessionToken = %q", hashToken(tok), h)
	}
	// Known SHA-256 of "abc".
	const abc = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if hashToken("abc") != abc {
		t.Fatalf("hashToken(abc) = %q, want %q", hashToken("abc"), abc)
	}
}

func TestSessionTokenHash_RejectsMalformed(t *testing.T) {
	good, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"empty":         "",
		"uuid form":     "6f1c1f5e-8a2b-4d43-9c55-0e4f7c1d2b3a",
		"not base64":    strings.Repeat("!", 43),
		"std padding":   base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"one short":     good[:42],
		"one long":      good + "A",
		"10 KB":         strings.Repeat("A", 10*1024),
		"hash as token": HashSessionToken(good),
		"embedded nul":  good[:42] + "\x00",
	}
	for name, tok := range cases {
		if h, ok := sessionTokenHash(tok); ok || h != "" {
			t.Errorf("%s: sessionTokenHash = (%q, %v), want rejected", name, h, ok)
		}
	}
	h, ok := sessionTokenHash(good)
	if !ok || h != HashSessionToken(good) {
		t.Fatalf("good token: (%q, %v)", h, ok)
	}
}

// A malformed cookie must be refused before any query: a nil pool would
// panic if ValidateSession or DeleteSession touched it.
func TestValidateSession_MalformedCookieNeverQueries(t *testing.T) {
	for _, tok := range []string{"", "not-a-session", strings.Repeat("x", 10*1024)} {
		u, err := ValidateSession(context.Background(), nil, tok)
		if u != nil || !errors.Is(err, ErrSessionInvalid) {
			t.Fatalf("ValidateSession(%.20q) = (%v, %v), want ErrSessionInvalid", tok, u, err)
		}
		if err := DeleteSession(context.Background(), nil, tok); err != nil {
			t.Fatalf("DeleteSession(%.20q) = %v, want nil (nothing to delete)", tok, err)
		}
	}
}

func closedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("building pool: %v", err)
	}
	pool.Close()
	return pool
}

func TestValidateSession_ClosedPoolIsAnErrorNotInvalid(t *testing.T) {
	tok, err := newSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	u, err := ValidateSession(context.Background(), closedPool(t), tok)
	if u != nil || err == nil {
		t.Fatalf("ValidateSession on a closed pool = (%v, %v), want an error", u, err)
	}
	if !strings.Contains(err.Error(), "validating session") {
		t.Fatalf("error %q does not name the step", err)
	}
	if errors.Is(err, ErrSessionInvalid) {
		t.Fatal("a database failure must not look like an invalid session")
	}
	if err := DeleteSession(context.Background(), closedPool(t), tok); err == nil ||
		!strings.Contains(err.Error(), "deleting session") {
		t.Fatalf("DeleteSession on a closed pool = %v, want a deleting session error", err)
	}
}

func TestCreateSession_ClosedPool(t *testing.T) {
	tok, err := CreateSessionWithDuration(context.Background(), closedPool(t), 1, SessionDuration)
	if tok != "" || err == nil || !strings.Contains(err.Error(), "creating session") {
		t.Fatalf("= (%q, %v), want a creating session error and no token", tok, err)
	}
}

type fakeExecer struct {
	sql  string
	args []any
	rows int64
	err  error
}

func (f *fakeExecer) Exec(
	_ context.Context, sql string, args ...any,
) (pgconn.CommandTag, error) {
	f.sql, f.args = sql, args
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	return pgconn.NewCommandTag("DELETE " + strconv.FormatInt(f.rows, 10)), nil
}

func TestEndUserSessions_ReturnsCountAndPassesExcept(t *testing.T) {
	f := &fakeExecer{rows: 3}
	n, err := EndUserSessions(context.Background(), f, 42, "keep-me")
	if err != nil || n != 3 {
		t.Fatalf("= (%d, %v), want (3, nil)", n, err)
	}
	if !strings.Contains(f.sql, "DELETE FROM sage.sessions WHERE user_id") {
		t.Fatalf("unexpected SQL %q", f.sql)
	}
	if len(f.args) != 2 || f.args[0] != 42 || f.args[1] != "keep-me" {
		t.Fatalf("args = %v, want [42 keep-me]", f.args)
	}
}

func TestEndUserSessions_InvalidUserNeverQueries(t *testing.T) {
	for _, id := range []int{0, -1} {
		f := &fakeExecer{}
		n, err := EndUserSessions(context.Background(), f, id, "")
		if err == nil || n != 0 || f.sql != "" {
			t.Fatalf("user %d: = (%d, %v), sql %q; want an error and no query", id, n, err, f.sql)
		}
	}
	if n, err := EndUserSessions(context.Background(), nil, 1, ""); err == nil || n != 0 {
		t.Fatalf("nil querier: = (%d, %v), want an error", n, err)
	}
}

func TestEndUserSessions_PropagatesError(t *testing.T) {
	cause := errors.New("connection refused")
	n, err := EndUserSessions(context.Background(), &fakeExecer{err: cause}, 7, "")
	if n != 0 || !errors.Is(err, cause) || !strings.Contains(err.Error(), "ending sessions") {
		t.Fatalf("= (%d, %v), want a wrapped ending sessions error", n, err)
	}
}

func TestSessionAuditEventNames(t *testing.T) {
	if AuditLogout != "logout" || AuditSessionsRevoked != "sessions_revoked" {
		t.Fatalf("audit names = %q, %q", AuditLogout, AuditSessionsRevoked)
	}
}
