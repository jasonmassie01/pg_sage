package executor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
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
	requested, isKeepSize, err := parseWALKeepSizeBytes(sql)
	if !isKeepSize || err != nil || requested < 0 {
		return err
	}
	var retained int64
	if err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(max(
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
