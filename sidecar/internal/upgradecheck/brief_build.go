package upgradecheck

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// briefFacts are what the brief reads beyond the checks, in the run's
// read-only transaction: each read under its own savepoint, so one that
// fails leaves only its section unknown.
type briefFacts struct {
	installed    []extension
	installedErr error
	// available is the target member's versions per extension; nil is
	// unknown, with availableWhy.
	available    map[string][]string
	availableWhy string
	reindex      []ReindexStep
	reindexTotal int
	reindexErr   error
}

// reindexSQL: indexes with a key whose collation's recorded version
// differs from the library's (the U5 collation rows), as table and index.
// $1 is the row limit.
const reindexSQLHead = Tag + `SELECT pg_catalog.format('%I.%I', n.nspname, ic.relname),
  pg_catalog.format('%I.%I', tn.nspname, tc.relname),
  pg_catalog.string_agg(DISTINCT co.collname::text, ',' ORDER BY co.collname::text),
  count(*) OVER ()
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_namespace n ON n.oid = ic.relnamespace
JOIN pg_catalog.pg_class tc ON tc.oid = i.indrelid
JOIN pg_catalog.pg_namespace tn ON tn.oid = tc.relnamespace
JOIN pg_catalog.unnest(i.indcollation::oid[]) AS k(coll) ON true
JOIN pg_catalog.pg_collation co ON co.oid = k.coll
WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname !~ '^pg_toast'
  AND ((co.collversion IS NOT NULL
        AND co.collversion IS DISTINCT FROM pg_catalog.pg_collation_actual_version(co.oid))`

// reindexDefaultArm (PostgreSQL 15+): keys on the database's default
// collation when the database's recorded collation version changed.
const reindexDefaultArm = `
    OR (co.oid = 100 AND EXISTS (SELECT 1 FROM pg_catalog.pg_database d
      WHERE d.datname = current_database() AND d.datcollversion IS NOT NULL
        AND d.datcollversion IS DISTINCT FROM
            pg_catalog.pg_database_collation_actual_version(d.oid)))`

const reindexSQLTail = `)
GROUP BY n.nspname, ic.relname, tn.nspname, tc.relname
ORDER BY 1
LIMIT $1`

func reindexSQL(major int) string {
	if major >= 15 {
		return reindexSQLHead + reindexDefaultArm + reindexSQLTail
	}
	return reindexSQLHead + reindexSQLTail
}

// BuildBrief runs the readiness checks for t (Major 0 is
// DefaultTargetMajor) and builds the brief from that one run, in its
// read-only transaction. It fails as Run does.
func BuildBrief(ctx context.Context, pool *pgxpool.Pool, t Target, opts BriefOptions) (Brief,
	error) {
	if pool == nil {
		return Brief{}, ErrNoPool
	}
	if t.Major == 0 {
		t.Major = DefaultTargetMajor(opts.NewestOfferedMajor)
	}
	var facts briefFacts
	run := opts.Options
	run.brief = func(ctx context.Context, tx pgx.Tx, in input) {
		facts = readBriefFacts(ctx, tx, in)
	}
	rep, err := Run(ctx, pool, t, run)
	if err != nil {
		return Brief{}, err
	}
	if ctx.Err() != nil { // a section read cut short is not an unknown
		return Brief{}, fmt.Errorf("upgrade brief: %w", ctx.Err())
	}
	return newBrief(rep, facts, opts), nil
}

func readBriefFacts(ctx context.Context, tx pgx.Tx, in input) briefFacts {
	var f briefFacts
	f.installedErr = catalogread.InSavepoint(ctx, tx, func(sp pgx.Tx) error {
		var err error
		f.installed, err = readInstalled(ctx, sp)
		return err
	})
	if f.installedErr == nil && len(f.installed) > 0 {
		f.available, f.availableWhy = targetVersions(ctx, in, f.installed)
	}
	f.reindexErr = catalogread.InSavepoint(ctx, tx, func(sp pgx.Tx) error {
		rows, total, err := collect(ctx, sp, []string{"index", "table", "collations"},
			reindexSQL(in.src.major))
		if err != nil {
			return err
		}
		f.reindexTotal = total
		for _, r := range rows {
			f.reindex = append(f.reindex, ReindexStep{Action: ReindexAction,
				Index: r["index"], Table: r["table"],
				Collations: strings.Split(r["collations"], ","),
				Reason: "the collation's recorded version differs from the library's: " +
					"keys may sort differently"})
		}
		return nil
	})
	return f
}

// targetVersions reads what the target member offers, as U2 does; nil
// with the reason when there is no member or it is not at the target.
func targetVersions(ctx context.Context, in input, installed []extension) (
	map[string][]string, string) {
	if in.opts.TargetDB == nil {
		return nil, "no target_database was named: name a fleet member already at " +
			"PostgreSQL " + strconv.Itoa(in.target.Major) + "; pg_sage does not guess " +
			"what the target ships"
	}
	available, major, err := readAvailable(ctx, in.opts.TargetDB, in.opts.timeout(),
		installed)
	if err != nil {
		return nil, "the target database could not be read: " + err.Error()
	}
	if major != in.target.Major {
		return nil, fmt.Sprintf("target_database %s runs PostgreSQL %d, not the target %d",
			in.opts.TargetDatabase, major, in.target.Major)
	}
	return available, ""
}
