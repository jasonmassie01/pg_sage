package collector

import "github.com/pg-sage/sidecar/internal/extschema"

// pgssNames are pg_stat_statements' objects qualified by the extension's
// schema (extschema), so an extension outside the search_path, or a
// search_path pinned to pg_catalog, pg_temp, is still read.
type pgssNames struct {
	view string // pg_stat_statements
	info string // pg_stat_statements_info (1.9+)
	fn   string // pg_stat_statements(showtext)
}

// newPgssNames qualifies the objects by schema; an empty schema (the
// extension is not installed, or not yet found) is the undefined_table
// error the bare names raised.
func newPgssNames(schema string) (pgssNames, error) {
	view, err := extschema.Rel(schema, "pg_stat_statements")
	if err != nil {
		return pgssNames{}, err
	}
	info, err := extschema.Rel(schema, "pg_stat_statements_info")
	if err != nil {
		return pgssNames{}, err
	}
	fn, err := extschema.Func(schema, "pg_stat_statements")
	return pgssNames{view: view, info: info, fn: fn}, err
}

// statementsSchema is pg_stat_statements' schema: the last successful
// capability probe's, or the startup checks' (cfg.StatementsSchema) until
// a probe has found it. A probe re-reads it each cycle, so an extension
// created or moved after startup is followed on the next refresh.
func (c *Collector) statementsSchema() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.caps.probed {
		return c.caps.key.schema
	}
	return c.cfg.StatementsSchema
}

// pgss returns the qualified names for this cycle's statements reads. It
// does not probe: the counters read probes first (queryCapabilities), and
// the other reads use the schema the last probe or the startup checks
// found.
func (c *Collector) pgss() (pgssNames, error) {
	return newPgssNames(c.statementsSchema())
}
