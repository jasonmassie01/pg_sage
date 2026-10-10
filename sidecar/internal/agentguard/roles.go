package agentguard

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MinRoleServerVersion is the first server_version_num where Guard
// manages roles (PostgreSQL 16, §6.6).
const MinRoleServerVersion = 160000

// transactionTimeoutVersion is the first server with transaction_timeout.
const transactionTimeoutVersion = 170000

// RoleConfig is the attributes and per-database settings of agent roles
// (agents.roles.* and agents.broker.pool_max_conns, §9).
type RoleConfig struct {
	// ConnectionLimit bounds the direct-lane role (agents.roles.connection_limit).
	ConnectionLimit int
	// BrokerConnectionLimit bounds the broker login (agents.broker.pool_max_conns).
	BrokerConnectionLimit    int
	StatementTimeout         time.Duration
	LockTimeout              time.Duration
	IdleInTransactionTimeout time.Duration
	IdleSessionTimeout       time.Duration
	TransactionTimeout       time.Duration // PG17+
	TempFileLimitMB          int           // where pg_sage may set it (G1-13)
	BrokerCredentialRotation time.Duration // agents.broker.rotation_days
	RoleChangeLockTimeout    time.Duration // the role transaction's own lock_timeout
}

// DefaultRoleConfig is the spec §9 defaults.
func DefaultRoleConfig() RoleConfig {
	return RoleConfig{ConnectionLimit: 5, BrokerConnectionLimit: 2,
		StatementTimeout: 30 * time.Second, LockTimeout: 2 * time.Second,
		IdleInTransactionTimeout: time.Minute, IdleSessionTimeout: 10 * time.Minute,
		TransactionTimeout: 10 * time.Minute, TempFileLimitMB: 1024,
		BrokerCredentialRotation: 7 * 24 * time.Hour, RoleChangeLockTimeout: 2 * time.Second}
}

// Validate checks every bound is positive.
func (c RoleConfig) Validate() error {
	switch {
	case c.ConnectionLimit < 1 || c.BrokerConnectionLimit < 1:
		return invalid("agent role connection limits must be at least 1")
	case c.StatementTimeout <= 0 || c.LockTimeout <= 0 || c.IdleInTransactionTimeout <= 0:
		return invalid("agent role timeouts must be positive")
	case c.IdleSessionTimeout <= 0 || c.TransactionTimeout <= 0:
		return invalid("agent role session timeouts must be positive")
	case c.TempFileLimitMB < 0 || c.RoleChangeLockTimeout <= 0:
		return invalid("temp_file_limit and the role lock timeout must not be negative")
	case c.BrokerCredentialRotation <= 0:
		return invalid("broker credential rotation must be positive")
	}
	return nil
}

// Setting is one ALTER ROLE … IN DATABASE … SET parameter.
type Setting struct {
	Name  string
	Value string
	// Optional settings are applied where pg_sage may set them; a refusal
	// (42501) is recorded, not fatal (temp_file_limit, G1-13).
	Optional bool
}

// Settings is what every agent role gets in each database (§6.6).
func (c RoleConfig) Settings(serverVersion int) []Setting {
	ms := func(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }
	out := []Setting{{Name: "statement_timeout", Value: ms(c.StatementTimeout)},
		{Name: "lock_timeout", Value: ms(c.LockTimeout)},
		{Name: "idle_in_transaction_session_timeout", Value: ms(c.IdleInTransactionTimeout)},
		{Name: "idle_session_timeout", Value: ms(c.IdleSessionTimeout)}}
	if serverVersion >= transactionTimeoutVersion {
		out = append(out, Setting{Name: "transaction_timeout", Value: ms(c.TransactionTimeout)})
	}
	if c.TempFileLimitMB > 0 {
		out = append(out, Setting{Name: "temp_file_limit",
			Value: fmt.Sprintf("%dMB", c.TempFileLimitMB), Optional: true})
	}
	return out
}

// ClusterDatabase is one monitored database of a cluster, with pg_sage's
// own pool on it.
type ClusterDatabase struct {
	Name string
	Pool *pgxpool.Pool
}

// Cluster is where a principal's roles live (§6.6: roles are cluster-wide).
// Admin is pg_sage's pool on the database whose executor runs the role
// contracts and records them; Databases are the cluster's monitored
// databases with grants (each gets settings and CONNECT; retire runs
// DROP OWNED BY in each). Key is the provider resource id, else the
// system identifier.
type Cluster struct {
	Key       string
	Admin     *pgxpool.Pool
	Databases []ClusterDatabase
}

func (c Cluster) validate() error {
	if strings.TrimSpace(c.Key) == "" || len(c.Key) > 500 {
		return invalid("cluster key must be 1 to 500 characters")
	}
	if c.Admin == nil {
		return invalid("cluster %s has no pg_sage pool", c.Key)
	}
	seen := map[string]bool{}
	for _, d := range c.Databases {
		if d.Name == "" || d.Pool == nil || seen[d.Name] {
			return invalid("cluster %s: database entries need a unique name and a pool",
				c.Key)
		}
		seen[d.Name] = true
	}
	return nil
}

// serverVersion reads server_version_num.
func serverVersion(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (int, error) {
	var v int
	err := q.QueryRow(ctx, `/* pg_sage guard_role v1 */
		SELECT current_setting('server_version_num')::int`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("agentguard: reading server version: %w", err)
	}
	return v, nil
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// literal quotes a string literal; values are generated (verifiers,
// durations), never caller text.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// alterAttributes is what ALTER ROLE re-asserts. A CREATEROLE role may not
// name SUPERUSER, CREATEDB, REPLICATION or BYPASSRLS unless it holds them
// itself (PostgreSQL 16), so those are set only at CREATE ROLE; drift in
// them fails the post-check and needs a superuser to fix.
func alterAttributes(login bool, limit int) string {
	l := "NOLOGIN"
	if login {
		l = "LOGIN"
	}
	return fmt.Sprintf("%s NOCREATEROLE CONNECTION LIMIT %d", l, limit)
}

// roleAttributes is the attribute list every agent role is created with.
func roleAttributes(login bool, limit int) string {
	l := "NOLOGIN"
	if login {
		l = "LOGIN"
	}
	return fmt.Sprintf("%s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS "+
		"CONNECTION LIMIT %d", l, limit)
}

// lockKey is the advisory lock that serializes changes to one principal's
// roles across sidecars (the contracts' "cluster role" lease target).
func lockKey(principalID string) string { return "pg_sage guard_role:" + principalID }
