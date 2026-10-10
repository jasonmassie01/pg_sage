package agentguard

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Replicas (§6.10 step 6, GR-01): the kill connects to each configured
// replica (databases[].replicas) to end agent sessions there. Standbys
// only seen in pg_stat_replication have no routable DSN: they are named in
// the report with the bound within which their agent sessions end; new
// logins fail there as soon as the primary's NOLOGIN replays.

// replicaRun is one configured replica during a kill.
type replicaRun struct {
	target  string // the monitored database that configured it
	replica Replica
	conn    *pgx.Conn
	cluster string // its cluster_name, which the walreceiver reports
	report  ReplicaReport
}

func (s *Switch) openReplica(ctx context.Context, target string, r Replica) *replicaRun {
	run := &replicaRun{target: target, replica: r,
		report: ReplicaReport{Name: r.Name, Configured: true}}
	if r.DSN == "" {
		run.report.Error = "no DSN: the replica's dsn_env is not set"
		return run
	}
	cfg, err := pgx.ParseConfig(r.DSN)
	if err != nil {
		run.report.Error = "the replica's DSN does not parse"
		return run
	}
	cfg.ConnectTimeout = s.cfg.ReplicaConnectTimeout
	cfg.RuntimeParams["application_name"] = "pg_sage agent kill"
	cctx, cancel := context.WithTimeout(ctx, s.cfg.ReplicaConnectTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(cctx, cfg)
	if err != nil {
		run.report.Error = "unreachable: " + connectError(err)
		return run
	}
	run.conn = conn
	if err := conn.QueryRow(ctx, `/* pg_sage guard_kill v1 */
		SELECT current_setting('cluster_name')`).Scan(&run.cluster); err != nil {
		run.cluster = ""
	}
	return run
}

// connectError describes a connection failure. pgconn's message names the
// user, host and database, never the password.
func connectError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "connection timed out"
	}
	return err.Error()
}

func (r *replicaRun) close() {
	if r.conn != nil {
		_ = r.conn.Close(context.Background())
	}
}

// contain ends agent backends on the replica and checks logins fail.
func (r *replicaRun) contain(ctx context.Context, k killScope, datname string) {
	if r.conn == nil {
		return
	}
	n, err := terminate(ctx, r.conn, k, datname)
	r.report.BackendsTerminated += n
	if err != nil {
		r.report.Error = joinErr(r.report.Error, err)
	}
}

// check verifies the replica: no agent backend left and, unless the kill
// leaves roles alone (scope database), every agent role NOLOGIN there.
func (r *replicaRun) check(ctx context.Context, k killScope, datname string) bool {
	if r.conn == nil {
		return false
	}
	n, err := countAgents(ctx, r.conn, k, datname)
	if err != nil {
		r.report.Error = joinErr(r.report.Error, err)
		return false
	}
	if n > 0 {
		r.contain(ctx, k, datname)
		return false
	}
	blocked := true
	if k.disablesRoles() {
		blocked, err = loginsBlocked(ctx, r.conn, k)
		if err != nil {
			r.report.Error = joinErr(r.report.Error, err)
			return false
		}
	}
	r.report.LoginsBlocked = blocked
	r.report.Verified = blocked && r.report.Error == ""
	return r.report.Verified
}

// loginsBlocked reports whether every agent role in scope is NOLOGIN on
// q's server (on a standby: once the primary's change replayed).
func loginsBlocked(ctx context.Context, q rowQuery, k killScope) (bool, error) {
	var blocked bool
	err := q.QueryRow(ctx, `/* pg_sage guard_kill v1 */
		SELECT coalesce(bool_and(NOT rolcanlogin AND rolconnlimit = 0), true)
		FROM pg_catalog.pg_roles
		WHERE rolname = ANY($1) OR ($2 AND rolname ~ '`+RoleRegex+`')`,
		k.roles(), k.all).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("agentguard: reading agent role logins: %w", err)
	}
	return blocked, nil
}

// observeStandbys reads pg_stat_replication on q's server. It needs
// pg_read_all_stats (pg_monitor) to see application names.
func observeStandbys(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, source string) ([]StandbyObservation, error) {
	rows, err := q.Query(ctx, `/* pg_sage guard_kill v1 */
		SELECT coalesce(application_name, ''), coalesce(client_addr::text, ''),
			coalesce(state, '') FROM pg_catalog.pg_stat_replication`)
	if err != nil {
		return nil, fmt.Errorf("agentguard: reading pg_stat_replication on %s: %w", source,
			err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StandbyObservation, error) {
		o := StandbyObservation{Source: source}
		return o, r.Scan(&o.ApplicationName, &o.ClientAddr, &o.State)
	})
}
