package executor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// ErrUnsafeGUCValue is returned when an allowlisted GUC value would harm
// the cluster (e.g. a WAL keep size that invalidates live slots).
var ErrUnsafeGUCValue = errors.New("unsafe GUC value")

var walKeepSizePattern = regexp.MustCompile(
	`(?i)^\s*ALTER\s+SYSTEM\s+SET\s+max_slot_wal_keep_size\s*(?:=|TO)\s*'?\s*` +
		`(-?[0-9]+)\s*(kB|MB|GB|TB)?\s*'?\s*;?\s*$`)

// parseWALKeepSizeBytes extracts the bytes an ALTER SYSTEM SET
// max_slot_wal_keep_size statement requests (-1 = unlimited). ok is false
// for other statements; err is set when the value cannot be parsed.
func parseWALKeepSizeBytes(sql string) (int64, bool, error) {
	upper := strings.ToUpper(normalizeSQLText(sql))
	if !strings.HasPrefix(upper, "ALTER SYSTEM SET MAX_SLOT_WAL_KEEP_SIZE") {
		return 0, false, nil
	}
	match := walKeepSizePattern.FindStringSubmatch(normalizeSQLText(sql))
	if len(match) != 3 {
		return 0, true, fmt.Errorf("%w: cannot parse max_slot_wal_keep_size", ErrUnsafeGUCValue)
	}
	value, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, true, fmt.Errorf("%w: %v", ErrUnsafeGUCValue, err)
	}
	if value < 0 {
		return -1, true, nil
	}
	shift := map[string]uint{"KB": 10, "MB": 20, "GB": 30, "TB": 40, "": 20}
	return value << shift[strings.ToUpper(match[2])], true, nil
}

// checkGUCValueSafety refuses a max_slot_wal_keep_size below 1.5x the WAL
// any replication slot currently retains: such a value invalidates the
// slot at the next checkpoint (replica rebuild / CDC re-snapshot).
func (e *Executor) checkGUCValueSafety(ctx context.Context, sql string) error {
	if err := e.checkManagedDatabaseSetting(ctx, sql); err != nil {
		return err
	}
	requested, isKeepSize, err := parseWALKeepSizeBytes(sql)
	if !isKeepSize || err != nil || requested < 0 {
		return err
	}
	var retained int64
	if err := e.pool.QueryRow(ctx, `/* pg_sage executor */ SELECT COALESCE(max(
		pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)), 0)::bigint
		FROM pg_replication_slots`).Scan(&retained); err != nil {
		return fmt.Errorf("%w: cannot read retained WAL: %v", ErrUnsafeGUCValue, err)
	}
	if requested < retained+retained/2 {
		return fmt.Errorf("%w: max_slot_wal_keep_size %d bytes is below 1.5x the "+
			"%d bytes a slot retains", ErrUnsafeGUCValue, requested, retained)
	}
	return nil
}

// checkManagedDatabaseSetting refuses, on every managed provider, an ALTER
// DATABASE ... SET/RESET that PostgreSQL would refuse: only user-context
// settings, and superuser-context ones the connected role may SET, can be
// set per database (advisors-01). It runs before the statement, so a stale
// finding written before the managed rewrite routed on context never
// reaches PostgreSQL. Unknown catalog state fails closed.
func (e *Executor) checkManagedDatabaseSetting(ctx context.Context, sql string) error {
	cfg, _, _ := e.policySnapshot()
	if cfg == nil || !isManagedProvider(cfg.CloudEnvironment) {
		return nil
	}
	upper := strings.ToUpper(normalizeSQLText(sql))
	if !strings.HasPrefix(upper, "ALTER DATABASE") {
		return nil
	}
	provider := strings.TrimSpace(cfg.CloudEnvironment)
	clause, ok := alterDatabaseClause(upper)
	stmt, parsed := pgconf.ParseAlterSystem("ALTER SYSTEM " + clause)
	if !ok || !parsed || e.pool == nil {
		return fmt.Errorf("%w: cannot establish the %s database setting context for %q",
			ErrManagedConfigUnsupported, provider, sql)
	}
	settable, settingContext, err := databaseSettable(ctx, e.pool, stmt.Name)
	if err != nil {
		return fmt.Errorf("%w: cannot establish %s setting %s context: %v",
			ErrManagedConfigUnsupported, provider, stmt.Name, err)
	}
	if !settable {
		return fmt.Errorf("%w: %s setting %s has %s context; only user-context settings "+
			"(and superuser-context ones the role may SET) can be set per database",
			ErrManagedConfigUnsupported, provider, stmt.Name, settingContext)
	}
	return nil
}

// databaseSettable reads a setting's pg_settings context and whether the
// connected role may set it per database: user context always; superuser
// context for a superuser or, on PostgreSQL 15+, a role holding SET on the
// parameter. A setting pg_settings does not list is an error.
func databaseSettable(ctx context.Context, pool *pgxpool.Pool, name string) (bool, string, error) {
	var settingContext string
	var super bool
	var version int
	if err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT s.context, r.rolsuper,
		current_setting('server_version_num')::int
		FROM pg_catalog.pg_settings s, pg_catalog.pg_roles r
		WHERE s.name = $1 AND r.rolname = current_user`, name).
		Scan(&settingContext, &super, &version); err != nil {
		return false, "", err
	}
	switch {
	case settingContext == "user":
		return true, settingContext, nil
	case settingContext != "superuser":
		return false, settingContext, nil
	case super:
		return true, settingContext, nil
	case version < 150000:
		return false, settingContext, nil // no parameter privileges before PostgreSQL 15
	}
	var granted bool
	if err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT
		pg_catalog.has_parameter_privilege($1, 'SET')`, name).Scan(&granted); err != nil {
		return false, settingContext, fmt.Errorf("SET privilege on %s: %w", name, err)
	}
	return granted, settingContext, nil
}
