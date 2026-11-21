package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The GitOps directory reconciler's grant surface (cli-02a): grants a file
// under policy.directory owns are source 'config'. The reconciler writes
// only those rows, in its own transaction, and the admin editor and the
// SSO mapping never change them (an admin edits the file instead).

// GrantSourceConfig marks a grant a GitOps file owns.
const GrantSourceConfig = "config"

// AuditConfigReconciled is the one audit entry of a GitOps reconcile that
// wrote anything; its detail carries the directory hash and the counts.
const AuditConfigReconciled = "config_directory_reconciled"

// ErrConfigGrantReadOnly is an admin's change of a grant a GitOps file
// owns.
var ErrConfigGrantReadOnly = errors.New(
	"database grant is set by a file under policy.directory; edit the file")

// GrantUser is a sage.users row a grant file may name.
type GrantUser struct {
	ID       int
	Email    string
	Role     string
	Disabled bool
}

// GrantUsersTx lists the users by lower-cased email.
func GrantUsersTx(ctx context.Context, tx pgx.Tx) (map[string]GrantUser, error) {
	rows, err := tx.Query(ctx, `SELECT id, email, role, disabled_at IS NOT NULL
		FROM sage.users ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading users for the config reconcile: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GrantUser, error) {
		var u GrantUser
		err := row.Scan(&u.ID, &u.Email, &u.Role, &u.Disabled)
		return u, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading users for the config reconcile: %w", err)
	}
	out := make(map[string]GrantUser, len(list))
	for _, u := range list {
		out[strings.ToLower(u.Email)] = u
	}
	return out, nil
}

// AllGrantsTx lists every stored grant, locked for the rest of tx.
func AllGrantsTx(ctx context.Context, tx pgx.Tx) ([]DatabaseGrant, error) {
	rows, err := tx.Query(ctx, `SELECT user_id, database_name, role, granted_by,
		granted_at, source FROM sage.user_database_grants
		ORDER BY user_id, database_name FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("reading grants for the config reconcile: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DatabaseGrant, error) {
		var g DatabaseGrant
		err := row.Scan(&g.UserID, &g.Database, &g.Role, &g.GrantedBy, &g.GrantedAt,
			&g.Source)
		return g, err
	})
}

// SetConfigGrantTx creates, changes or adopts c's grant as source
// 'config' and audits it with actor (the file) in the detail.
func SetConfigGrantTx(ctx context.Context, tx pgx.Tx, c GrantChange, actor string) error {
	if !IsGrantRole(c.Role) {
		return ErrInvalidGrantRole
	}
	if !validGrantDatabase(c.Database) {
		return ErrInvalidGrantDatabase
	}
	prev, _, err := lockGrant(ctx, tx, c)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sage.user_database_grants
		(user_id, database_name, role, granted_by, source)
		VALUES ($1, $2, $3, 0, 'config')
		ON CONFLICT (user_id, database_name) DO UPDATE
		SET role = EXCLUDED.role, granted_by = 0, granted_at = now(), source = 'config'`,
		c.UserID, c.Database, c.Role); err != nil {
		return fmt.Errorf("writing config grant of user %d on %s: %w", c.UserID,
			c.Database, err)
	}
	ev := grantAudit(AuditDatabaseGrantSet, c, prev)
	ev.Detail["source"], ev.Detail["actor"] = GrantSourceConfig, actor
	return RecordAuthAudit(ctx, tx, ev)
}

// RevokeConfigGrantTx removes c's grant when a file owns it.
func RevokeConfigGrantTx(ctx context.Context, tx pgx.Tx, c GrantChange, actor string) error {
	prev, source, err := lockGrant(ctx, tx, c)
	if err != nil {
		return err
	}
	if prev == "" || source != GrantSourceConfig {
		return fmt.Errorf("%w: config grant of user %d on %s", ErrGrantNotFound, c.UserID,
			c.Database)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sage.user_database_grants
		WHERE user_id = $1 AND database_name = $2 AND source = 'config'`,
		c.UserID, c.Database); err != nil {
		return fmt.Errorf("deleting config grant of user %d on %s: %w", c.UserID,
			c.Database, err)
	}
	ev := grantAudit(AuditDatabaseGrantRevoked, c, prev)
	ev.Detail["source"], ev.Detail["actor"] = GrantSourceConfig, actor
	return RecordAuthAudit(ctx, tx, ev)
}
