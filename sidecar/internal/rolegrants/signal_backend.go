package rolegrants

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SignalNeed is what pg_signal_backend is for.
const SignalNeed = "cancelling a runaway query when an action needs it"

// SignalBackend is whether the connected role may cancel other roles'
// queries. pg_cancel_backend checks the privileges of pg_signal_backend
// (has_privs_of_role) on every supported version, so a member that does
// not inherit them (WITH INHERIT FALSE on PostgreSQL 16+, a NOINHERIT role
// before) cannot cancel: Usable is false and MemberOnly true.
type SignalBackend struct {
	Usable     bool
	MemberOnly bool
	// Version is server_version_num; it picks the SQL that fixes MemberOnly.
	Version int
}

// signalBackendSQL tests USAGE (the inherited privileges, which signalling
// checks) and MEMBER (only to tell a member that does not inherit them).
const signalBackendSQL = `/* pg_sage grants */ SELECT
  pg_catalog.pg_has_role(current_user, r.oid, 'USAGE'),
  pg_catalog.pg_has_role(current_user, r.oid, 'MEMBER'),
  pg_catalog.current_setting('server_version_num')::int
FROM pg_catalog.pg_roles r WHERE r.rolname = 'pg_signal_backend'`

// CheckSignalBackend reads, live, whether the connected role may use
// pg_signal_backend.
func CheckSignalBackend(ctx context.Context, pool *pgxpool.Pool) (SignalBackend, error) {
	if pool == nil {
		return SignalBackend{}, ErrNoPool
	}
	var s SignalBackend
	var member bool
	if err := pool.QueryRow(ctx, signalBackendSQL).Scan(&s.Usable, &member,
		&s.Version); err != nil {
		return SignalBackend{}, fmt.Errorf("read pg_signal_backend privileges: %w", err)
	}
	s.MemberOnly = member && !s.Usable
	return s, nil
}

// GrantSQL is the statement that lets role (already quoted) use
// pg_signal_backend. Re-running a plain GRANT does not change an existing
// membership's INHERIT option, so a member that does not inherit gets
// WITH INHERIT TRUE (PostgreSQL 16+) or, before 16, the role's INHERIT
// attribute.
func (s SignalBackend) GrantSQL(role string) string {
	switch {
	case s.MemberOnly && s.Version >= 160000:
		return "GRANT pg_signal_backend TO " + role + " WITH INHERIT TRUE"
	case s.MemberOnly:
		return "ALTER ROLE " + role + " INHERIT"
	}
	return "GRANT pg_signal_backend TO " + role
}
