package analyzer

import (
	"context"
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// Extension health rules (market-16): pg_cron and pg_partman keep state
// that nothing else reads. Each rule runs only when its extension is
// installed in the monitored database, and reads the schema the gate
// found, quoted, never an assumed one.
const (
	extPgCron  = "pg_cron"
	extPartman = "pg_partman"

	categoryCronJobFailed  = "pg_cron_job_failed"
	categoryCronHistory    = "pg_cron_history_unbounded"
	categoryPartmanPremake = "partman_premake_gap"
	categoryPartmanDefault = "partman_default_rows"
)

// extCategories are the extension rules' categories; each rule has the
// name of its one category.
var extCategories = []string{categoryCronJobFailed, categoryCronHistory,
	categoryPartmanPremake, categoryPartmanDefault}

var (
	cronCategories    = []string{categoryCronJobFailed, categoryCronHistory}
	partmanCategories = []string{categoryPartmanPremake, categoryPartmanDefault}
)

// isExtCategory reports whether c belongs to an extension health rule
// (market-16 or TimescaleDB, market-16b).
func isExtCategory(c string) bool {
	return slices.Contains(extCategories, c) || slices.Contains(tsCategories, c)
}

// extGateNames are the extensions the gate looks for.
var extGateNames = []string{extPgCron, extPartman, extTimescale}

// extInstall is where an extension keeps its objects. For pg_cron, whose
// extension schema is pg_catalog, it is the schema of its job table.
type extInstall struct {
	Schema  string
	Version string
}

// extGateFunc reads the installed extensions the rules need.
type extGateFunc func(ctx context.Context, q catalogread.Querier) (map[string]extInstall, error)

// extReading is one cycle's reading of the extension rules' inputs; a nil
// part means the extension is absent or its rules are off this cycle.
type extReading struct {
	cron      *cronReading
	partman   *partmanReading
	timescale *tsReading
}

// extGateSQL finds pg_cron, pg_partman and TimescaleDB and the schema of
// a relation each owns (job, part_config, job_stats), falling back to the
// extension's schema.
const extGateSQL = `/* pg_sage analyzer */
SELECT e.extname, e.extversion,
       COALESCE((SELECT tn.nspname
                   FROM pg_depend d
                   JOIN pg_class c ON c.oid = d.objid
                   JOIN pg_namespace tn ON tn.oid = c.relnamespace
                  WHERE d.classid = 'pg_class'::regclass
                    AND d.refclassid = 'pg_extension'::regclass
                    AND d.refobjid = e.oid AND d.deptype = 'e'
                    AND c.relname = CASE e.extname WHEN 'pg_cron' THEN 'job'
                                                   WHEN 'timescaledb' THEN 'job_stats'
                                                   ELSE 'part_config' END
                  LIMIT 1), n.nspname) AS schema
FROM pg_extension e
JOIN pg_namespace n ON n.oid = e.extnamespace
WHERE e.extname = ANY($1)`

// readExtensionGate is the one gate query each cycle runs.
func readExtensionGate(ctx context.Context, q catalogread.Querier) (
	map[string]extInstall, error,
) {
	rows, err := q.Query(ctx, extGateSQL, extGateNames)
	if err != nil {
		return nil, fmt.Errorf("read pg_extension: %w", err)
	}
	defer rows.Close()
	out := map[string]extInstall{}
	for rows.Next() {
		var name string
		var in extInstall
		if err := rows.Scan(&name, &in.Version, &in.Schema); err != nil {
			return nil, fmt.Errorf("scan pg_extension: %w", err)
		}
		out[name] = in
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pg_extension: %w", err)
	}
	return out, nil
}

// loadExtensionState reads this cycle's extension inputs through the
// bounded catalog reader.
func (a *Analyzer) loadExtensionState(ctx context.Context) {
	if a.pool == nil {
		a.extras.ext = nil
		a.evalFail(extCategories...)
		a.evalFail(tsCategories...)
		return
	}
	a.readExtensions(ctx, a.catalog())
}

// readExtensions runs the gate and, for each installed extension whose
// rules are on, its reads. A category that could not be read is marked
// not evaluated, so its open findings stay open; with no extension
// installed nothing runs beyond the gate and no category is evaluated.
func (a *Analyzer) readExtensions(ctx context.Context, q catalogread.Querier) {
	a.extras.ext = nil
	gate := a.extGate
	if gate == nil {
		gate = readExtensionGate
	}
	installs, err := gate(ctx, q)
	if err != nil {
		a.extWarn("gate", "analyzer: extension rules: %v; pg_cron, pg_partman and "+
			"TimescaleDB checks not evaluated until pg_extension can be read", err)
		a.evalFail(extCategories...)
		a.evalFail(tsCategories...)
		return
	}
	a.extOK("gate")
	r := &extReading{}
	if in, ok := installs[extPgCron]; ok && a.extRulesOn(cronCategories) {
		r.cron = a.readCron(ctx, q, in.Schema)
	} else {
		a.evalFail(cronCategories...)
	}
	if in, ok := installs[extPartman]; ok && a.extRulesOn(partmanCategories) {
		r.partman = a.readPartman(ctx, q, in.Schema)
	} else {
		a.evalFail(partmanCategories...)
	}
	if in, ok := installs[extTimescale]; ok && a.extRulesOn(tsCategories) {
		r.timescale = a.readTimescale(ctx, q, in.Schema)
	} else {
		a.evalFail(tsCategories...)
	}
	a.extras.ext = r
}

// extRulesOn reports whether any of the categories is enabled this cycle;
// a disabled rule's reads are skipped (it resolves as rule_disabled).
func (a *Analyzer) extRulesOn(categories []string) bool {
	e := a.cyclePack()
	if e == nil {
		return true
	}
	for _, c := range categories {
		if !e.disabled[c] {
			return true
		}
	}
	return false
}

// extWarn logs a failed extension read once per streak of failures.
func (a *Analyzer) extWarn(key, format string, args ...any) {
	if a.extras.extWarned == nil {
		a.extras.extWarned = map[string]bool{}
	}
	if a.extras.extWarned[key] {
		return
	}
	a.extras.extWarned[key] = true
	a.logFn("WARN", format, args...)
}

// extOK ends a streak of failures for key.
func (a *Analyzer) extOK(key string) { delete(a.extras.extWarned, key) }
