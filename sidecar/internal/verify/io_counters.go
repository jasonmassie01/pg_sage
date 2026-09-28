package verify

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// IOQueryer is the read surface ReadIOCounters needs.
type IOQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Data-IO counter queries, one row per comparable counter part.
//
// PG16/17: pg_stat_io reads, writes and extends of relation data, times
// op_bytes. Extends are writes that grow a relation (bulk loads and index
// builds), so they count as write IO.
//
// PG18: op_bytes was replaced by read_bytes/write_bytes/extend_bytes, and
// pg_stat_io gained object='wal' rows, which stay excluded here because WAL
// is measured by pg_stat_wal.
//
// PG14/15 (no pg_stat_io): pg_stat_database.blks_read per database plus the
// buffers written by the checkpointer, bgwriter and backends from
// pg_stat_bgwriter, times block_size. blks_read includes reads served by the
// OS page cache, so this over-states device reads; the learned baseline
// compares the database with itself, which keeps that bias consistent.
const (
	pgStatIOOpBytesQuery = `SELECT backend_type || '|' || object || '|' || context,
		((COALESCE(reads, 0) + COALESCE(writes, 0) + COALESCE(extends, 0))
			* COALESCE(op_bytes, 0))::float8,
		COALESCE(stats_reset::text, '')
		FROM pg_stat_io WHERE object IN ('relation', 'temp relation')`
	pgStatIOBytesQuery = `SELECT backend_type || '|' || object || '|' || context,
		(COALESCE(read_bytes, 0) + COALESCE(write_bytes, 0)
			+ COALESCE(extend_bytes, 0))::float8,
		COALESCE(stats_reset::text, '')
		FROM pg_stat_io WHERE object IN ('relation', 'temp relation')`
	pgStatDatabaseQuery = `SELECT 'db:' || datid::text,
		COALESCE(blks_read, 0)::float8 * current_setting('block_size')::float8,
		COALESCE(stats_reset::text, '')
		FROM pg_stat_database
		UNION ALL
		SELECT 'bgwriter',
		(buffers_checkpoint + buffers_clean + buffers_backend)::float8
			* current_setting('block_size')::float8,
		COALESCE(stats_reset::text, '')
		FROM pg_stat_bgwriter`
	walCounterQuery = `SELECT wal_bytes::float8, COALESCE(stats_reset::text, '')
		FROM pg_stat_wal`
)

// dataIOQuery returns the version-appropriate data-IO counter query and the
// source it measures.
func dataIOQuery(serverVersionNum int) (string, string, error) {
	switch {
	case serverVersionNum >= 180000:
		return pgStatIOBytesQuery, IOSourcePGStatIO, nil
	case serverVersionNum >= 160000:
		return pgStatIOOpBytesQuery, IOSourcePGStatIO, nil
	case serverVersionNum >= 140000:
		return pgStatDatabaseQuery, IOSourcePGStatDatabase, nil
	default:
		return "", "", fmt.Errorf(
			"pg-side IO evidence requires PostgreSQL 14 or later (pg_stat_wal); server is %d",
			serverVersionNum)
	}
}

// ReadIOCounters reads the cumulative pg-side data-IO and WAL counters.
func ReadIOCounters(ctx context.Context, q IOQueryer) (IOCounters, error) {
	if q == nil {
		return IOCounters{}, errors.New("IO counters need a database connection")
	}
	var version int
	if err := q.QueryRow(ctx,
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		return IOCounters{}, fmt.Errorf("read server version: %w", err)
	}
	query, source, err := dataIOQuery(version)
	if err != nil {
		return IOCounters{}, err
	}
	parts, err := readCounterParts(ctx, q, query)
	if err != nil {
		return IOCounters{}, fmt.Errorf("read %s counters: %w", source, err)
	}
	counters := IOCounters{Source: source, DataParts: parts}
	if err := q.QueryRow(ctx, walCounterQuery).Scan(
		&counters.WAL.Bytes, &counters.WAL.Reset); err != nil {
		return IOCounters{}, fmt.Errorf("read pg_stat_wal: %w", err)
	}
	return counters, nil
}

func readCounterParts(
	ctx context.Context, q IOQueryer, query string,
) (map[string]IOCounterPart, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := make(map[string]IOCounterPart)
	for rows.Next() {
		var key string
		var part IOCounterPart
		if err := rows.Scan(&key, &part.Bytes, &part.Reset); err != nil {
			return nil, err
		}
		parts[key] = part
	}
	return parts, rows.Err()
}
