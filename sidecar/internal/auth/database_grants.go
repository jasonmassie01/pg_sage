package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Per-database grants (apiauth-08). A user with no grants keeps their
// global role on every database. A user with any grant is scoped: on a
// granted database their role is the lower of the global role and the
// grant, and everywhere else (and on fleet-wide actions) they are a
// viewer. An admin is never scoped. Every grant change is written to
// sage.auth_audit, which the audit chain seals, in the same transaction.

// Database grant audit events (apiauth-08). Detail carries the database,
// the role and the previous role; a refusal carries the database and the
// surface that refused it.
const (
	AuditDatabaseGrantSet     = "database_grant_set"
	AuditDatabaseGrantRevoked = "database_grant_revoked"
	AuditDatabaseForbidden    = "database_forbidden"
)

var (
	// ErrGrantStoreUnavailable is a grant lookup that could not be made;
	// callers refuse the request (fail closed).
	ErrGrantStoreUnavailable = errors.New("database grant store unavailable")
	ErrInvalidGrantRole      = errors.New("a database grant role must be viewer or operator")
	ErrInvalidGrantDatabase  = errors.New("a database grant must name one database")
	ErrGrantUserNotFound     = errors.New("database grant: user not found")
	ErrGrantNotFound         = errors.New("database grant not found")
	// ErrIdPGrantReadOnly is a revoke of a grant the SSO group mapping
	// owns while that mapping is configured (apiauth-08d).
	ErrIdPGrantReadOnly = errors.New("database grant is set by the SSO group mapping")
)

// Grant sources (apiauth-08d): who owns a stored grant.
const (
	GrantSourceManual = "manual"
	GrantSourceIdP    = "idp"
)

var roleRanks = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// IsGrantRole reports a role a database grant may carry.
func IsGrantRole(role string) bool {
	return role == RoleViewer || role == RoleOperator
}

// RoleAtLeast reports whether role meets min; unknown roles fail closed.
func RoleAtLeast(role, min string) bool {
	have, want := roleRanks[role], roleRanks[min]
	return have > 0 && want > 0 && have >= want
}

// EffectiveDatabaseRole is a user's role on database ("" names an action
// that spans databases) given their global role and grants. An unknown
// global role, or an unknown grant role on the database, yields "".
func EffectiveDatabaseRole(globalRole string, grants map[string]string,
	database string) string {
	if roleRanks[globalRole] == 0 {
		return ""
	}
	if globalRole == RoleAdmin || len(grants) == 0 {
		return globalRole
	}
	grant, ok := grants[database]
	if database == "" || !ok {
		return RoleViewer
	}
	if roleRanks[grant] == 0 {
		return ""
	}
	if roleRanks[grant] < roleRanks[globalRole] {
		return grant
	}
	return globalRole
}

// DatabaseGrant is one stored grant.
type DatabaseGrant struct {
	UserID    int       `json:"user_id"`
	Database  string    `json:"database"`
	Role      string    `json:"role"`
	GrantedBy int       `json:"granted_by"`
	GrantedAt time.Time `json:"granted_at"`
	Source    string    `json:"source"` // GrantSourceManual, GrantSourceIdP or GrantSourceConfig
}

// GrantChange is one grant write: who makes it, for whom, on what.
type GrantChange struct {
	ActorID  int
	UserID   int
	Database string
	Role     string // ignored by RevokeGrant
	SourceIP string
	// ProtectIdP makes RevokeGrant refuse a grant the SSO group mapping
	// owns: set while oauth.database_grant_mapping is configured.
	ProtectIdP bool
}

// DatabaseGrantStore reads and writes sage.user_database_grants.
type DatabaseGrantStore struct{ pool *pgxpool.Pool }

// NewDatabaseGrantStore returns a store on the control database pool.
func NewDatabaseGrantStore(pool *pgxpool.Pool) *DatabaseGrantStore {
	return &DatabaseGrantStore{pool: pool}
}

func (s *DatabaseGrantStore) ready() error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("%w: no control database pool", ErrGrantStoreUnavailable)
	}
	return nil
}

// UserDatabaseGrants returns userID's grants as database → role; an empty
// map means the user is not scoped.
func (s *DatabaseGrantStore) UserDatabaseGrants(ctx context.Context,
	userID int) (map[string]string, error) {
	list, err := s.ListUserGrants(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, g := range list {
		out[g.Database] = g.Role
	}
	return out, nil
}

// ListUserGrants returns userID's grants ordered by database name.
func (s *DatabaseGrantStore) ListUserGrants(ctx context.Context,
	userID int) ([]DatabaseGrant, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id, database_name, role, granted_by,
		granted_at, source FROM sage.user_database_grants WHERE user_id = $1
		ORDER BY database_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading grants of user %d: %v",
			ErrGrantStoreUnavailable, userID, err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DatabaseGrant, error) {
		var g DatabaseGrant
		err := row.Scan(&g.UserID, &g.Database, &g.Role, &g.GrantedBy, &g.GrantedAt,
			&g.Source)
		return g, err
	})
	if err != nil {
		return nil, fmt.Errorf("%w: reading grants of user %d: %v",
			ErrGrantStoreUnavailable, userID, err)
	}
	return list, nil
}

func validGrantDatabase(name string) bool {
	return name != "" && name != "all" && name != "*"
}

// SetGrant creates or changes c.UserID's grant on c.Database and audits it.
// The grant is manual: an admin's choice, which takes over an IdP grant on
// the same database and is never changed by an SSO login.
func (s *DatabaseGrantStore) SetGrant(ctx context.Context, c GrantChange) (DatabaseGrant,
	error) {
	if !IsGrantRole(c.Role) {
		return DatabaseGrant{}, ErrInvalidGrantRole
	}
	if !validGrantDatabase(c.Database) {
		return DatabaseGrant{}, ErrInvalidGrantDatabase
	}
	if err := s.ready(); err != nil {
		return DatabaseGrant{}, err
	}
	var g DatabaseGrant
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		prev, source, err := lockGrant(ctx, tx, c)
		if err != nil {
			return err
		}
		if source == GrantSourceConfig {
			return ErrConfigGrantReadOnly
		}
		g = DatabaseGrant{UserID: c.UserID, Database: c.Database, Role: c.Role,
			GrantedBy: c.ActorID, Source: GrantSourceManual}
		if err := tx.QueryRow(ctx, `INSERT INTO sage.user_database_grants
			(user_id, database_name, role, granted_by, source)
			VALUES ($1, $2, $3, $4, 'manual')
			ON CONFLICT (user_id, database_name) DO UPDATE
			SET role = EXCLUDED.role, granted_by = EXCLUDED.granted_by, granted_at = now(),
				source = 'manual'
			RETURNING granted_at`, c.UserID, c.Database, c.Role, c.ActorID).
			Scan(&g.GrantedAt); err != nil {
			return fmt.Errorf("writing grant of user %d on %s: %w", c.UserID, c.Database, err)
		}
		return RecordAuthAudit(ctx, tx, grantAudit(AuditDatabaseGrantSet, c, prev))
	})
	if err != nil {
		return DatabaseGrant{}, err
	}
	return g, nil
}

// RevokeGrant removes c.UserID's grant on c.Database and audits it.
// Revoking a user's last grant returns them to their global role. With
// c.ProtectIdP a grant the SSO group mapping owns is refused.
func (s *DatabaseGrantStore) RevokeGrant(ctx context.Context, c GrantChange) error {
	if !validGrantDatabase(c.Database) {
		return ErrInvalidGrantDatabase
	}
	if err := s.ready(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		prev, source, err := lockGrant(ctx, tx, c)
		if err != nil {
			return err
		}
		if prev == "" {
			return ErrGrantNotFound
		}
		if c.ProtectIdP && source == GrantSourceIdP {
			return ErrIdPGrantReadOnly
		}
		if source == GrantSourceConfig {
			return ErrConfigGrantReadOnly
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sage.user_database_grants
			WHERE user_id = $1 AND database_name = $2`, c.UserID, c.Database); err != nil {
			return fmt.Errorf("deleting grant of user %d on %s: %w", c.UserID, c.Database, err)
		}
		return RecordAuthAudit(ctx, tx, grantAudit(AuditDatabaseGrantRevoked, c, prev))
	})
}

// lockGrant locks the target user's row (so a concurrent delete waits)
// and returns their current role and its source on the database ("" when
// none).
func lockGrant(ctx context.Context, tx pgx.Tx, c GrantChange) (string, string, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM sage.users WHERE id = $1 FOR KEY SHARE`,
		c.UserID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrGrantUserNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("locking user %d for a grant change: %w", c.UserID, err)
	}
	var prev, source string
	err = tx.QueryRow(ctx, `SELECT role, source FROM sage.user_database_grants
		WHERE user_id = $1 AND database_name = $2 FOR UPDATE`, c.UserID, c.Database).
		Scan(&prev, &source)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("reading grant of user %d on %s: %w", c.UserID,
			c.Database, err)
	}
	return prev, source, nil
}

func grantAudit(event string, c GrantChange, prev string) AuthAuditEvent {
	detail := map[string]any{"database": c.Database, "previous_role": prev}
	if event == AuditDatabaseGrantSet {
		detail["role"] = c.Role
	}
	return AuthAuditEvent{Event: event, ActorUserID: c.ActorID, TargetUserID: c.UserID,
		SourceIP: c.SourceIP, Detail: detail}
}
