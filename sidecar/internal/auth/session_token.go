package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Session cookies are 32 random bytes, base64url without padding. Only
// their SHA-256 (hex) is stored, in sage.sessions.token_hash, so reading
// the sage schema (pg_read_all_data, a dump, a restored backup) does not
// yield a usable session (apiauth-04).
const sessionTokenBytes = 32

var sessionTokenLen = base64.RawURLEncoding.EncodedLen(sessionTokenBytes)

// ErrSessionInvalid is an unknown, expired or malformed session cookie. Any
// other ValidateSession error is a failure of the session store itself.
var ErrSessionInvalid = errors.New("session invalid or expired")

// ErrSessionStoreUnavailable is a session lookup the store could not answer
// (no pool, a refused or dropped connection, a timeout). It says nothing
// about the cookie, so callers answer 503 and keep the user signed in
// (apiauth-02).
var ErrSessionStoreUnavailable = errors.New("session store unavailable")

// Execer runs one statement; *pgxpool.Pool, *pgx.Conn and pgx.Tx satisfy it.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func newSessionToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken is the stored form of a bearer secret (sessions, link grants).
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HashSessionToken returns the stored form of a session cookie value.
func HashSessionToken(token string) string {
	return hashToken(token)
}

// sessionTokenHash returns the stored hash of a well-formed cookie, and
// false (with no hash) for anything that cannot be a session cookie, so a
// malformed or oversized cookie never reaches the database.
func sessionTokenHash(token string) (string, bool) {
	if len(token) != sessionTokenLen {
		return "", false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != sessionTokenBytes {
		return "", false
	}
	return hashToken(token), true
}

// EndUserSessions deletes userID's sessions except the one whose stored
// hash is exceptHash (empty ends them all) and returns how many it ended.
// It is the one place that ends a user's sessions: unlinking SSO, the
// Databricks admin bootstrap and "sign out everywhere else" use it.
func EndUserSessions(
	ctx context.Context, q Execer, userID int, exceptHash string,
) (int, error) {
	if q == nil {
		return 0, fmt.Errorf("ending sessions for user %d: no database", userID)
	}
	if userID <= 0 {
		return 0, fmt.Errorf("ending sessions: invalid user id %d", userID)
	}
	tag, err := q.Exec(ctx,
		"/* pg_sage auth_end_sessions v1 */ "+
			"DELETE FROM sage.sessions WHERE user_id = $1 "+
			"AND ($2::text = '' OR token_hash <> $2::text)",
		userID, exceptHash)
	if err != nil {
		return 0, fmt.Errorf("ending sessions for user %d: %w", userID, err)
	}
	return int(tag.RowsAffected()), nil
}
