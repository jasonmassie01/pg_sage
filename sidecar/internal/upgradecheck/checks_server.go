package upgradecheck

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// checksumsDefaultMajor is the first major whose initdb enables data
// checksums by default; pg_upgrade refuses clusters that differ.
const checksumsDefaultMajor = 18

var checksumsSQL = Tag + `SELECT current_setting('data_checksums')`

// U4: data checksums off on the source while the target's initdb turns
// them on: pg_upgrade into a default PG18 initdb fails on the mismatch.
func checkChecksums(ctx context.Context, q pgx.Tx, in input) (Check, error) {
	var setting string
	if err := q.QueryRow(ctx, checksumsSQL).Scan(&setting); err != nil {
		return Check{}, err
	}
	row := []Row{{"data_checksums": setting}}
	out := Check{Status: Pass, Rows: row, Total: 1}
	switch {
	case setting == "on":
		out.Reason = "data checksums are on, as the target's initdb default is"
	case in.target.Major < checksumsDefaultMajor:
		out.Reason = "data checksums are off, and initdb of PostgreSQL " +
			strconv.Itoa(in.target.Major) + " leaves them off by default too"
	case in.target.Method == PGUpgrade:
		out.Status = Block
		out.Reason = "data checksums are off, but initdb of PostgreSQL 18 and later " +
			"enables them, and pg_upgrade refuses clusters whose checksum settings differ. " +
			"Either initialise the new cluster with initdb --no-data-checksums, or enable " +
			"checksums on the old cluster first with pg_checksums --enable while it is " +
			"stopped (it rewrites every data file)."
	default:
		out.Status = Warn
		out.Reason = "data checksums are off; a new PostgreSQL 18+ cluster has them on " +
			"by default, which a " + in.target.Method + " upgrade allows but which costs " +
			"some write throughput: decide before you initialise it."
	}
	return out, nil
}

// ftsProviderMajor is the first major whose full text search and pg_trgm
// use the database's collation provider (not libc) for case folding.
const ftsProviderMajor = 18

var dbCollationSQL = Tag + `SELECT d.datname::text, d.datcollversion::text,
  pg_catalog.pg_database_collation_actual_version(d.oid)::text, count(*) OVER ()
FROM pg_catalog.pg_database d
WHERE d.datname = current_database() AND d.datcollversion IS NOT NULL
  AND d.datcollversion IS DISTINCT FROM pg_catalog.pg_database_collation_actual_version(d.oid)
LIMIT $1`

var icuCollationSQL = Tag + `SELECT c.collname::text, c.collversion::text,
  pg_catalog.pg_collation_actual_version(c.oid)::text, count(*) OVER ()
FROM pg_catalog.pg_collation c
WHERE c.collprovider = 'i' AND c.collversion IS NOT NULL
  AND c.collversion IS DISTINCT FROM pg_catalog.pg_collation_actual_version(c.oid)
ORDER BY 1
LIMIT $1`

// ftsIndexSQL: GIN and GiST indexes with a pg_trgm operator class or a
// tsvector expression column; their keys depend on case folding. An
// index column's pg_attribute type is its operator class's storage type
// (text for GIN tsvector_ops, gtsvector for GiST), so a tsvector column
// is found by its operator class's input type; indkey 0 is an expression.
var ftsIndexSQL = Tag + `SELECT pg_catalog.format('%I.%I', n.nspname, ic.relname),
  am.amname::text, count(*) OVER ()
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_namespace n ON n.oid = ic.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = ic.relam
WHERE am.amname IN ('gin', 'gist')
  AND (EXISTS (SELECT 1 FROM pg_catalog.pg_opclass oc
         WHERE oc.oid = ANY (i.indclass::oid[])
           AND oc.opcname IN ('gin_trgm_ops', 'gist_trgm_ops'))
    OR EXISTS (SELECT 1 FROM pg_catalog.generate_subscripts(i.indclass, 1) AS k(n)
         JOIN pg_catalog.pg_opclass oc ON oc.oid = i.indclass[k.n]
         WHERE oc.opcintype = 'pg_catalog.tsvector'::pg_catalog.regtype
           AND i.indkey[k.n] = 0))
ORDER BY 1
LIMIT $1`

// U5: collation versions that changed under the database, and on a
// non-libc database the indexes a PG18+ target folds case differently
// for (reindex them after the upgrade).
func checkCollations(ctx context.Context, q pgx.Tx, in input) (Check, error) {
	out := Check{Status: Pass, Rows: []Row{},
		Reason: "recorded collation versions match the library's, and no index " +
			"depends on a provider change"}
	arms := []struct {
		kind, skip string
		cols       []string
		sql        string
	}{
		{"database", dbCollationArm(in), []string{"name", "recorded", "actual"},
			dbCollationSQL},
		{"collation", "", []string{"name", "recorded", "actual"}, icuCollationSQL},
		{"index", ftsIndexArm(in), []string{"name", "access_method"}, ftsIndexSQL},
	}
	for _, arm := range arms {
		if arm.skip != "" {
			out.Skipped = append(out.Skipped, arm.kind+": "+arm.skip)
			continue
		}
		rows, total, err := collect(ctx, q, arm.cols, arm.sql)
		if err != nil {
			return Check{}, err
		}
		out.Total += total
		for _, r := range rows {
			r["kind"] = arm.kind
			if len(out.Rows) < MaxRows {
				out.Rows = append(out.Rows, r)
			}
		}
	}
	if out.Total > 0 {
		out.Status = Warn
		out.Reason = "collation versions changed under indexes, or indexes depend on " +
			"PostgreSQL 18's full text search and pg_trgm case folding by the database's " +
			"provider: REINDEX them after the upgrade (and ALTER DATABASE/COLLATION ... " +
			"REFRESH VERSION once rebuilt)."
	}
	return out, nil
}

func dbCollationArm(in input) string {
	if in.src.major < 15 {
		return "PostgreSQL " + strconv.Itoa(in.src.major) + " records no database " +
			"collation version (datcollversion is PostgreSQL 15+)"
	}
	return ""
}

func ftsIndexArm(in input) string {
	switch {
	case in.target.Major < ftsProviderMajor:
		return "the target is below PostgreSQL 18, which keeps libc case folding for " +
			"full text search and pg_trgm"
	case in.src.locProvider == "c":
		return "the database uses the libc provider, so case folding does not change"
	}
	return ""
}

// removedSetting is a parameter a major removed or renamed.
type removedSetting struct {
	name    string
	removed int // the first major without it
	note    string
}

// removedSettings is from each major's release notes ("Migration to
// Version N"), read 2026-11-12.
var removedSettings = []removedSetting{
	{"stats_temp_directory", 15, "removed in PostgreSQL 15 (statistics live in " +
		"shared memory)"},
	{"force_parallel_mode", 16, "renamed to debug_parallel_query in PostgreSQL 16"},
	{"vacuum_defer_cleanup_age", 16, "removed in PostgreSQL 16; use " +
		"hot_standby_feedback or a replication slot"},
	{"promote_trigger_file", 16, "removed in PostgreSQL 16; promote with pg_ctl " +
		"promote or pg_promote()"},
	{"old_snapshot_threshold", 17, "removed in PostgreSQL 17"},
	{"db_user_namespace", 17, "removed in PostgreSQL 17"},
}

// settingsSQL reads pg_settings.source, not pg_file_settings: managed
// providers hide the latter.
var settingsSQL = Tag + `SELECT s.name::text, s.setting::text, s.source::text,
  count(*) OVER ()
FROM pg_catalog.pg_settings s
WHERE s.name = ANY($2) AND s.source NOT IN ('default', 'override')
ORDER BY 1
LIMIT $1`

// U6: non-default settings the target no longer accepts.
func checkSettings(ctx context.Context, q pgx.Tx, in input) (Check, error) {
	notes := map[string]string{}
	var names []string
	for _, r := range removedSettings {
		if in.src.major < r.removed && r.removed <= in.target.Major {
			notes[r.name] = r.note
			names = append(names, r.name)
		}
	}
	out := Check{Status: Pass, Rows: []Row{}}
	if len(names) == 0 {
		out.Reason = fmt.Sprintf("pg_sage knows of no setting removed between "+
			"PostgreSQL %d and %d", in.src.major, in.target.Major)
		return out, nil
	}
	rows, total, err := collect(ctx, q, []string{"name", "setting", "source"}, settingsSQL,
		names)
	if err != nil {
		return Check{}, err
	}
	out.Rows, out.Total = rows, total
	out.Reason = "no removed or renamed setting is set"
	for _, r := range rows {
		r["change"] = notes[r["name"]]
	}
	if total > 0 {
		out.Status = Warn
		out.Reason = "these settings are set but the target does not accept them: remove " +
			"or rename them in the configuration (and ALTER SYSTEM/DATABASE/ROLE ... RESET) " +
			"before the new server starts"
	}
	return out, nil
}
