package tuner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// spillSettingsTimeout bounds the per-cycle catalog read.
const spillSettingsTimeout = 5 * time.Second

// spillSettingsSQL reads, for each role, the work_mem its sessions in this
// database start with: the role-in-database setting, then the role's, then
// the database's, then ALTER ROLE ALL's, then the server default.
// current_setting('work_mem') would be pg_sage's own session value, and
// pg_settings.reset_val is only the server default when pg_sage's own login
// role has no work_mem setting here and passed none as a startup option.
// Otherwise the server default is unknown (NULL; boot_val would ignore
// postgresql.conf and could size a hint below the role's real work_mem).
const spillSettingsSQL = `/* pg_sage tuner */
WITH db AS (
    SELECT oid FROM pg_catalog.pg_database WHERE datname = current_database()
), cfg AS (
    SELECT s.setrole, s.setdatabase,
           substring(c FROM position('=' IN c) + 1) AS value
      FROM pg_catalog.pg_db_role_setting s
     CROSS JOIN LATERAL unnest(s.setconfig) AS c
     WHERE s.setdatabase IN (0, (SELECT oid FROM db))
       AND split_part(c, '=', 1) = 'work_mem'
), dflt AS (
    SELECT CASE WHEN source IN ('user', 'database user', 'client')
                  OR EXISTS (SELECT 1 FROM cfg WHERE setrole =
                       (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = session_user))
                THEN NULL ELSE reset_val END AS value
      FROM pg_catalog.pg_settings WHERE name = 'work_mem'
)
SELECT r.userid,
       COALESCE(
         (SELECT value FROM cfg WHERE setrole = r.userid::oid AND setdatabase <> 0),
         (SELECT value FROM cfg WHERE setrole = r.userid::oid AND setdatabase = 0),
         (SELECT value FROM cfg WHERE setrole = 0 AND setdatabase <> 0),
         (SELECT value FROM cfg WHERE setrole = 0 AND setdatabase = 0),
         (SELECT value FROM dflt)) AS work_mem,
       current_setting('block_size')::int8 AS block_size
  FROM unnest($1::int8[]) AS r(userid)`

// readSpillSettings reads block_size and the roles' work_mem baselines; a
// role whose baseline is unknown (a masked server default) is left out, so
// nothing is sized for it.
func (t *Tuner) readSpillSettings(ctx context.Context, userIDs []int64) (spillSettings, error) {
	if t.pool == nil {
		return spillSettings{}, errors.New("read work_mem baselines: no database pool")
	}
	ctx, cancel := context.WithTimeout(ctx, spillSettingsTimeout)
	defer cancel()
	rows, err := t.pool.Query(ctx, spillSettingsSQL, userIDs)
	if err != nil {
		return spillSettings{}, fmt.Errorf("read work_mem baselines of roles %v: %w",
			userIDs, err)
	}
	defer rows.Close()
	out := spillSettings{WorkMemKB: make(map[int64]int64, len(userIDs))}
	for rows.Next() {
		var user int64
		var value *string
		if err := rows.Scan(&user, &value, &out.BlockSize); err != nil {
			return spillSettings{}, fmt.Errorf("scan work_mem baseline: %w", err)
		}
		if value == nil {
			continue
		}
		kb, err := parseMemoryKB(*value)
		if err != nil {
			return spillSettings{}, fmt.Errorf("work_mem baseline of role %d: %w", user, err)
		}
		out.WorkMemKB[user] = kb
	}
	if err := rows.Err(); err != nil {
		return spillSettings{}, fmt.Errorf("read work_mem baselines: %w", err)
	}
	return out, nil
}

// memoryUnitKB converts PostgreSQL memory units (case-sensitive) to kB.
var memoryUnitKB = map[string]float64{
	"B": 1.0 / 1024, "kB": 1, "MB": 1024, "GB": 1024 * 1024, "TB": 1024 * 1024 * 1024,
}

// parseMemoryKB parses a memory setting as PostgreSQL stores it in
// pg_db_role_setting or pg_settings: a bare number is kB (work_mem's base
// unit), units are case-sensitive and may follow a space.
func parseMemoryKB(value string) (int64, error) {
	v := strings.TrimSpace(value)
	i := len(v)
	for i > 0 && (v[i-1] < '0' || v[i-1] > '9') {
		i--
	}
	num, unit := strings.TrimSpace(v[:i]), strings.TrimSpace(v[i:])
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	mult := 1.0
	if unit != "" {
		m, ok := memoryUnitKB[unit]
		if !ok {
			return 0, fmt.Errorf("invalid memory unit %q in %q", unit, value)
		}
		mult = m
	}
	kb := math.Round(n * mult)
	if kb < 1 || kb > math.MaxInt64/2 {
		return 0, fmt.Errorf("memory value %q out of range", value)
	}
	return int64(kb), nil
}
