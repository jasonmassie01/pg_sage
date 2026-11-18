package runway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The member space reading is read-only and bounded: one read-only
// transaction under statement_timeout and lock_timeout, three statements,
// at most MXMemberSampleMax member lookups. next_multixact_id and
// next_multi_offset are as of the last checkpoint; the oldest multixact is
// the oldest datminmxid (by mxid_age, so wraparound is handled), at or
// after the oldest multixact whose members are still stored.
const (
	mxBoundSQL  = `SET LOCAL statement_timeout = '5s'; SET LOCAL lock_timeout = '1s'`
	mxGrantsSQL = `/* pg_sage runway:mx_members */ SELECT
		pg_catalog.has_function_privilege('pg_catalog.pg_control_checkpoint()', 'EXECUTE'),
		pg_catalog.has_function_privilege('pg_catalog.pg_get_multixact_members(xid)',
		    'EXECUTE'),
		pg_catalog.quote_ident(current_user)`
	mxControlSQL = `/* pg_sage runway:mx_members */
		SELECT c.next_multixact_id::text::int8, c.next_multi_offset::text::int8,
		       c.checkpoint_time,
		       (SELECT d.datminmxid::text::int8 FROM pg_catalog.pg_database d
		        ORDER BY pg_catalog.mxid_age(d.datminmxid) DESC LIMIT 1)
		FROM pg_catalog.pg_control_checkpoint() c`
	mxMembersSQL = `/* pg_sage runway:mx_members */
		SELECT (SELECT count(*) FROM pg_catalog.pg_get_multixact_members(s.id::text::xid))
		FROM pg_catalog.unnest($1::int8[]) WITH ORDINALITY AS s(id, n) ORDER BY s.n`
)

// The functions a reading needs, as the grant that would permit them.
const (
	mxControlFunc = "pg_catalog.pg_control_checkpoint()"
	mxMembersFunc = "pg_catalog.pg_get_multixact_members(xid)"
)

// sqlstateInsufficientPrivilege is permission denied.
const sqlstateInsufficientPrivilege = "42501"

// mxQuerier is what a reading runs on (a read-only pgx.Tx).
type mxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// measureMXMembers reads the member space in one bounded read-only
// transaction.
func measureMXMembers(ctx context.Context, pool *pgxpool.Pool) (MXMemberEstimate, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return MXMemberEstimate{}, fmt.Errorf("begin the read-only reading: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // read-only: nothing to keep
	if _, err := tx.Exec(ctx, mxBoundSQL); err != nil {
		return MXMemberEstimate{}, fmt.Errorf("bound the reading: %w", err)
	}
	return readMXMembers(ctx, tx)
}

// readMXMembers reads the checkpoint's multixact state and samples the
// members. A function the role may not execute makes the reading unknown,
// naming the grant; any other failure is an error.
func readMXMembers(ctx context.Context, q mxQuerier) (MXMemberEstimate, error) {
	var canControl, canMembers bool
	var role string
	if err := q.QueryRow(ctx, mxGrantsSQL).Scan(&canControl, &canMembers,
		&role); err != nil {
		return MXMemberEstimate{}, fmt.Errorf("read the multixact function grants: %w", err)
	}
	var missing []string
	if !canControl {
		missing = append(missing, mxControlFunc)
	}
	if !canMembers {
		missing = append(missing, mxMembersFunc)
	}
	if len(missing) > 0 {
		return mxUnknown(role, missing...), nil
	}
	c, err := readMXControl(ctx, q)
	if isPermissionDenied(err) {
		return mxUnknown(role, mxControlFunc), nil
	}
	if err != nil {
		return MXMemberEstimate{}, err
	}
	e := MXMemberEstimate{MXControl: c, Status: MXMeasured,
		Live: liveMultixacts(c.OldestMXID, c.NextMXID)}
	counts, err := readMemberCounts(ctx, q, sampleIDs(c.OldestMXID, e.Live,
		MXMemberSampleMax))
	if isPermissionDenied(err) {
		return mxUnknown(role, mxMembersFunc), nil
	}
	if err != nil {
		return MXMemberEstimate{}, err
	}
	e.Sampled = len(counts)
	e.MeanMembers, e.Members, e.StdErr = estimateMembers(e.Live, counts)
	return e, nil
}

func readMXControl(ctx context.Context, q mxQuerier) (MXControl, error) {
	var next, offset, oldest int64
	var at time.Time
	if err := q.QueryRow(ctx, mxControlSQL).Scan(&next, &offset, &at,
		&oldest); err != nil {
		return MXControl{}, fmt.Errorf("read the checkpoint's multixact state: %w", err)
	}
	return MXControl{NextMXID: uint32(next), NextOffset: uint32(offset),
		OldestMXID: uint32(oldest), CheckpointAt: at}, nil
}

// readMemberCounts is the member count of each id, in order.
func readMemberCounts(ctx context.Context, q mxQuerier, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, mxMembersSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("sample %d multixacts' members: %w", len(ids), err)
	}
	counts, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("sample %d multixacts' members: %w", len(ids), err)
	}
	if len(counts) != len(ids) {
		return nil, fmt.Errorf("sample %d multixacts' members: %d counts returned",
			len(ids), len(counts))
	}
	return counts, nil
}

// mxUnknown is a reading the role is not permitted to make.
func mxUnknown(role string, funcs ...string) MXMemberEstimate {
	grants := make([]string, 0, len(funcs))
	for _, f := range funcs {
		grants = append(grants, fmt.Sprintf("GRANT EXECUTE ON FUNCTION %s TO %s", f, role))
	}
	return MXMemberEstimate{Status: MXUnknown,
		Reason: fmt.Sprintf("role %s may not execute %s", role, strings.Join(funcs, " or ")),
		Grant:  strings.Join(grants, "; ")}
}

func isPermissionDenied(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == sqlstateInsufficientPrivilege
}
