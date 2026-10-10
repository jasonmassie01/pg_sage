package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
)

// Break-glass admin (E1, SF-4): a local admin for when the IdP is down.
// Its password lives only in configuration as a bcrypt hash; the account
// row has no password, so the normal login cannot use it.
const (
	// BreakGlassEmail is the dedicated account every break-glass login uses.
	BreakGlassEmail = "break-glass@pg-sage.local"
	// BreakGlassProvider marks the account in sage.users.oauth_provider.
	BreakGlassProvider = "break_glass"
	// BreakGlassSessionDuration is short: break-glass is for an outage,
	// not a working day.
	BreakGlassSessionDuration = time.Hour
)

var (
	// ErrBreakGlassDisabled means break-glass is off or has no hash.
	ErrBreakGlassDisabled = errors.New("break-glass login is not enabled")
	// ErrBreakGlassDenied is a wrong break-glass password.
	ErrBreakGlassDenied = errors.New("break-glass login denied")
	// ErrBreakGlassConflict means BreakGlassEmail belongs to an ordinary
	// account, which break-glass refuses to take over.
	ErrBreakGlassConflict = errors.New(
		"break-glass account email is held by another account")
)

// BreakGlassLogin checks password against the configured hash and returns
// the break-glass admin, creating the account or restoring its admin role
// as needed. The caller audits and alerts on every attempt.
func BreakGlassLogin(
	ctx context.Context, pool *pgxpool.Pool,
	cfg config.BreakGlassConfig, password string,
) (*User, error) {
	if !cfg.Enabled || cfg.PasswordHash == "" {
		return nil, ErrBreakGlassDisabled
	}
	if password == "" || !CheckPassword(cfg.PasswordHash, password) {
		return nil, ErrBreakGlassDenied
	}
	u := User{Email: BreakGlassEmail, Role: RoleAdmin}
	err := pool.QueryRow(ctx,
		`/* pg_sage auth_break_glass v1 */
		INSERT INTO sage.users (email, password, role, oauth_provider, last_login)
		VALUES ($1, NULL, 'admin', $2, now())
		ON CONFLICT (email) DO UPDATE SET role = 'admin', last_login = now()
		 WHERE sage.users.password IS NULL AND sage.users.oauth_issuer IS NULL
		   AND sage.users.oauth_provider = $2
		RETURNING id, created_at, last_login`,
		BreakGlassEmail, BreakGlassProvider,
	).Scan(&u.ID, &u.CreatedAt, &u.LastLogin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBreakGlassConflict
	}
	if err != nil {
		return nil, fmt.Errorf("break-glass account: %w", err)
	}
	return &u, nil
}

// CreateSessionWithDuration inserts a session that expires after d.
func CreateSessionWithDuration(
	ctx context.Context, pool *pgxpool.Pool, userID int, d time.Duration,
) (string, error) {
	if d <= 0 {
		return "", fmt.Errorf("creating session: duration must be positive")
	}
	var sessionID string
	err := pool.QueryRow(ctx,
		"/* pg_sage auth_session v1 */ INSERT INTO sage.sessions (user_id, expires_at) "+
			"VALUES ($1, $2) RETURNING id",
		userID, time.Now().Add(d),
	).Scan(&sessionID)
	if err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}
	return sessionID, nil
}
