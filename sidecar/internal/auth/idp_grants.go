package auth

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
)

// SSO group to per-database grant mapping (apiauth-08d). With
// oauth.database_grant_mapping set, every SSO login replaces the user's
// source='idp' grants with the ones their groups give, in one transaction
// with an audit row per change. Manual grants are never touched; on the
// same database a manual grant wins and the login records that the IdP
// grant was shadowed.

// AuditIdPGrantShadowed records an IdP grant not applied because an
// admin's manual grant holds that database. Detail carries the database,
// idp_role and manual_role.
const AuditIdPGrantShadowed = "idp_grant_shadowed"

// ErrIdPGrantSync is an SSO login whose database grants could not be
// written; the login is refused (fail closed) and nothing changes.
var ErrIdPGrantSync = errors.New("sso database grants could not be applied")

// DatabaseGrantMappingConfigured reports whether IdP groups decide grants.
func DatabaseGrantMappingConfigured(cfg *config.OAuthConfig) bool {
	return cfg != nil && len(cfg.DatabaseGrantMapping) > 0
}

// ResolveIdPGrants returns database → role for the grants groups give:
// exact, case-sensitive group match, the higher role when several groups
// grant the same database. It never returns nil.
func ResolveIdPGrants(cfg *config.OAuthConfig, groups []string) map[string]string {
	out := map[string]string{}
	if cfg == nil {
		return out
	}
	member := make(map[string]bool, len(groups))
	for _, g := range groups {
		member[g] = true
	}
	for _, m := range cfg.DatabaseGrantMapping {
		if member[m.Group] && roleRanks[m.Role] > roleRanks[out[m.Database]] {
			out[m.Database] = m.Role
		}
	}
	return out
}

// IdPGrantSync is one login's grant set for a user.
type IdPGrantSync struct {
	UserID   int
	Grants   map[string]string // database → viewer|operator
	SourceIP string
}

// IdPGrantResult lists, by database name in order, what a sync did.
type IdPGrantResult struct {
	Set      []string // IdP grants created or changed
	Revoked  []string // IdP grants removed
	Shadowed []string // IdP grants a manual grant holds
}

type storedGrant struct{ role, source string }

// SyncIdPGrants makes the user's IdP grants exactly in.Grants. Logins of
// one user serialize on their user row, so concurrent logins leave one
// login's set, never a mix. Any failure rolls back every change.
func (s *DatabaseGrantStore) SyncIdPGrants(ctx context.Context,
	in IdPGrantSync) (IdPGrantResult, error) {
	for db, role := range in.Grants {
		if !validGrantDatabase(db) {
			return IdPGrantResult{}, ErrInvalidGrantDatabase
		}
		if !IsGrantRole(role) {
			return IdPGrantResult{}, ErrInvalidGrantRole
		}
	}
	if err := s.ready(); err != nil {
		return IdPGrantResult{}, err
	}
	var res IdPGrantResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		res = IdPGrantResult{}
		current, err := lockIdPSync(ctx, tx, in.UserID)
		if err != nil {
			return err
		}
		if err := revokeStaleIdPGrants(ctx, tx, in, current, &res); err != nil {
			return err
		}
		return applyIdPGrants(ctx, tx, in, current, &res)
	})
	if err != nil {
		return IdPGrantResult{}, err
	}
	return res, nil
}

// lockIdPSync takes the user row FOR NO KEY UPDATE, which serializes SSO
// syncs of one user, then reads and locks their grants.
func lockIdPSync(ctx context.Context, tx pgx.Tx, userID int) (map[string]storedGrant, error) {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM sage.users WHERE id = $1 FOR NO KEY UPDATE`,
		userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGrantUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("locking user %d for an SSO grant sync: %w", userID, err)
	}
	rows, err := tx.Query(ctx, `SELECT database_name, role, source
		FROM sage.user_database_grants WHERE user_id = $1 FOR UPDATE`, userID)
	if err != nil {
		return nil, fmt.Errorf("reading grants of user %d: %w", userID, err)
	}
	defer rows.Close()
	out := map[string]storedGrant{}
	for rows.Next() {
		var db string
		var g storedGrant
		if err := rows.Scan(&db, &g.role, &g.source); err != nil {
			return nil, fmt.Errorf("reading grants of user %d: %w", userID, err)
		}
		out[db] = g
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading grants of user %d: %w", userID, err)
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// revokeStaleIdPGrants removes IdP grants the login no longer gives.
func revokeStaleIdPGrants(ctx context.Context, tx pgx.Tx, in IdPGrantSync,
	current map[string]storedGrant, res *IdPGrantResult) error {
	for _, db := range sortedKeys(current) {
		g := current[db]
		if _, keep := in.Grants[db]; keep || g.source != GrantSourceIdP {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sage.user_database_grants
			WHERE user_id = $1 AND database_name = $2 AND source = 'idp'`,
			in.UserID, db); err != nil {
			return fmt.Errorf("removing IdP grant of user %d on %s: %w", in.UserID, db, err)
		}
		if err := RecordAuthAudit(ctx, tx, idpGrantAudit(AuditDatabaseGrantRevoked, in, db,
			map[string]any{"previous_role": g.role})); err != nil {
			return err
		}
		res.Revoked = append(res.Revoked, db)
	}
	return nil
}

// applyIdPGrants writes the login's grants. A manual grant on the same
// database wins (it is shadowed, not changed), including one an admin
// wrote after this sync read the grants.
func applyIdPGrants(ctx context.Context, tx pgx.Tx, in IdPGrantSync,
	current map[string]storedGrant, res *IdPGrantResult) error {
	for _, db := range sortedKeys(in.Grants) {
		role, prev := in.Grants[db], current[db]
		if prev.source == GrantSourceIdP && prev.role == role {
			continue
		}
		written := false
		if prev.source != GrantSourceManual {
			var err error
			if written, err = upsertIdPGrant(ctx, tx, in.UserID, db, role); err != nil {
				return err
			}
		}
		ev, detail := AuditDatabaseGrantSet, map[string]any{"role": role,
			"previous_role": prev.role}
		if !written {
			ev, detail = AuditIdPGrantShadowed, map[string]any{"idp_role": role,
				"manual_role": prev.role}
		}
		if err := RecordAuthAudit(ctx, tx, idpGrantAudit(ev, in, db, detail)); err != nil {
			return err
		}
		if written {
			res.Set = append(res.Set, db)
		} else {
			res.Shadowed = append(res.Shadowed, db)
		}
	}
	return nil
}

// upsertIdPGrant writes an IdP grant unless a manual one holds the
// database; it reports whether it wrote.
func upsertIdPGrant(ctx context.Context, tx pgx.Tx, userID int, db, role string) (bool,
	error) {
	tag, err := tx.Exec(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role, granted_by, source) VALUES ($1, $2, $3, 0, 'idp')
		ON CONFLICT (user_id, database_name) DO UPDATE
		SET role = EXCLUDED.role, granted_by = 0, granted_at = now()
		WHERE sage.user_database_grants.source = 'idp'`, userID, db, role)
	if err != nil {
		return false, fmt.Errorf("writing IdP grant of user %d on %s: %w", userID, db, err)
	}
	return tag.RowsAffected() == 1, nil
}

// idpGrantAudit is a grant event of an SSO login: the user is both actor
// and target, as for the IdP role sync.
func idpGrantAudit(event string, in IdPGrantSync, db string,
	detail map[string]any) AuthAuditEvent {
	detail["database"] = db
	detail["source"] = GrantSourceIdP
	return AuthAuditEvent{Event: event, ActorUserID: in.UserID, TargetUserID: in.UserID,
		SourceIP: in.SourceIP, Detail: detail}
}
