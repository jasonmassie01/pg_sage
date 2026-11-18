package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/extstats"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/sqlident"
)

// maxIdentBytes is PostgreSQL's identifier limit (NAMEDATALEN - 1).
const maxIdentBytes = 63

// maxKeyColumns bounds an index's or a statistics object's columns.
const maxKeyColumns = 8

// template turns a resolved hypothesis target into a step of its action
// type; its SQL is rendered here, never taken from the model.
type template struct {
	action string
	render func(b *builder, t target) (rendered, error)
}

// rendered is a template's output: the statements, what the step
// measures and the proposal the ladder tests.
type rendered struct {
	sql, rollback string
	setting       string
	tables        []string
	metric        changeplan.Metric
	direction     changeplan.Direction
	targets       []packQuery
	proposal      Proposal
}

// templates are the hypothesis kinds a model may propose. A kind whose
// action type has no executor contract yet stays advice ("no_contract");
// a kind whose exact rollback needs a value the pack does not hold stays
// advice ("invalid_target").
var templates = map[string]template{
	"create_index_concurrently": {action: "create_index_concurrently", render: renderIndex},
	"create_statistics":         {action: "create_statistics", render: renderStatistics},
	"set_column_statistics":     {action: "set_column_statistics", render: renderNoPriorValue},
	"set_table_autovacuum":      {action: "set_table_autovacuum", render: renderNoPriorValue},
	"alter_database_guc":        {action: "alter_database_guc", render: renderDatabaseGUC},
	"set_role_timeout":          {action: "set_role_timeout", render: renderNoPriorValue},
	"set_role_connection_limit": {action: "set_role_connection_limit",
		render: renderNoPriorValue},
}

// templateKinds lists the kinds, for the "no template" advice.
func templateKinds() string {
	kinds := make([]string, 0, len(templates))
	for k := range templates {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return strings.Join(kinds, ", ")
}

// columnsOn resolves t's table and columns in the pack (1 to
// maxKeyColumns distinct columns the table has).
func (b *builder) columnsOn(t target, least int) error {
	if !t.hasColumns || t.key != "" {
		return refuse(ReasonInvalidTarget, "%s is not a table with a column list "+
			"(schema.table(column, ...))", t)
	}
	if n := len(t.columns); n < least || n > maxKeyColumns {
		return refuse(ReasonInvalidTarget, "%s lists %d columns, want %d to %d", t, n, least,
			maxKeyColumns)
	}
	if err := distinct(t.columns); err != nil {
		return err
	}
	fact, ok := b.lp.tables[t.table()]
	if !ok {
		return refuse(ReasonUnknownObject, "table %s is not in the evidence pack", t.table())
	}
	for _, c := range t.columns {
		if !slices.ContainsFunc(fact.Columns, func(f workpack.ColumnFact) bool {
			return f.Name == c
		}) {
			return refuse(ReasonUnknownObject, "column %s is not a column of %s in the "+
				"evidence pack", c, t.table())
		}
	}
	return nil
}

func renderIndex(b *builder, t target) (rendered, error) {
	if err := b.columnsOn(t, 1); err != nil {
		return rendered{}, err
	}
	quoted := make([]string, len(t.columns))
	for i, c := range t.columns {
		quoted[i] = sqlident.Ident(c)
	}
	name := objectName("sage_", t.relation, t.columns, "_idx")
	sql := "CREATE INDEX CONCURRENTLY " + sqlident.Ident(name) + " ON " +
		sqlident.Qualified(t.schema, t.relation) + " (" + strings.Join(quoted, ", ") + ")"
	r := rendered{sql: sql, tables: []string{t.table()},
		rollback: "DROP INDEX CONCURRENTLY IF EXISTS " + sqlident.Qualified(t.schema, name),
		metric:   changeplan.MetricBlocksPerCall, direction: changeplan.DirectionDown}
	r.targets = hottest(mentioning(b.lp.tableQueries(t.schema, t.relation), t.columns))
	r.proposal = Proposal{Table: t.table(), SQL: sql, Queries: queryInfos(r.targets)}
	return r, nil
}

func renderStatistics(b *builder, t target) (rendered, error) {
	if err := b.columnsOn(t, 2); err != nil {
		return rendered{}, err
	}
	quoted := make([]string, len(t.columns))
	for i, c := range t.columns {
		quoted[i] = sqlident.Ident(c)
	}
	name := objectName(extstats.NamePrefix, t.relation, t.columns, "")
	sql := "CREATE STATISTICS " + sqlident.Qualified(t.schema, name) +
		" (ndistinct, dependencies, mcv) ON " + strings.Join(quoted, ", ") + " FROM " +
		sqlident.Qualified(t.schema, t.relation)
	parsed, err := extstats.ParseCreate(sql)
	if err != nil {
		return rendered{}, refuse(ReasonInvalidTarget, "the statistics statement for %s is "+
			"not the form pg_sage runs: %v", t, err)
	}
	r := rendered{sql: sql, rollback: parsed.Rollback(), tables: []string{t.table()},
		metric: changeplan.MetricTimePerCall, direction: changeplan.DirectionDown}
	r.targets = hottest(mentioning(b.lp.tableQueries(t.schema, t.relation), t.columns))
	r.proposal = Proposal{Table: t.table(), SQL: sql, Queries: queryInfos(r.targets)}
	return r, nil
}

// rollbackUnits are the pg_settings units a database-level value can be
// written back with exactly (value then unit).
var rollbackUnits = map[string]bool{"": true, "kB": true, "MB": true, "GB": true,
	"ms": true, "s": true, "min": true}

func renderDatabaseGUC(b *builder, t target) (rendered, error) {
	if t.key == "" || t.relation != "" {
		return rendered{}, refuse(ReasonInvalidTarget, "%s is not one setting=value", t)
	}
	if pgconf.RequiresRestart(t.key) {
		return rendered{}, refuse(ReasonInvalidTarget, "%s takes effect only after a restart; "+
			"a database-level setting cannot change it", t.key)
	}
	fact, ok := b.lp.settings[t.key]
	if !ok {
		return rendered{}, refuse(ReasonUnknownObject, "setting %s is not in the evidence "+
			"pack", t.key)
	}
	if !pgconf.ExecutableGUC(t.key) {
		return rendered{}, refuse(ReasonInvalidTarget, "setting %s is not one the executor "+
			"may change", t.key)
	}
	if ok, why := pgconf.ValidateValue(t.key, t.value); !ok {
		return rendered{}, refuse(ReasonInvalidTarget, "%s=%s: %s", t.key, t.value, why)
	}
	db := "ALTER DATABASE " + sanitize.QuoteIdentifier(b.in.Database)
	rollback := db + " RESET " + t.key
	if fact.Source == "database" {
		if !rollbackUnits[fact.Unit] {
			return rendered{}, refuse(ReasonInvalidTarget, "the database sets %s to %s in "+
				"units of %s, which cannot be written back exactly", t.key, fact.Value,
				fact.Unit)
		}
		rollback = db + " SET " + t.key + " = " + sanitize.QuoteLiteral(fact.Value+fact.Unit)
	}
	sql := db + " SET " + t.key + " = " + sanitize.QuoteLiteral(t.value)
	return rendered{sql: sql, rollback: rollback, setting: t.key,
		metric: changeplan.MetricReadback, direction: changeplan.DirectionNone,
		proposal: Proposal{SQL: sql, Setting: t.key, Value: t.value,
			Queries: queryInfos(hottest(b.lp.queries))}}, nil
}

// renderNoPriorValue refuses a kind whose exact rollback needs the
// object's current value (a column's statistics target, a table's
// storage parameters, a role's setting or limit), which the evidence pack
// does not hold. Writing RESET instead could silently lose a value an
// operator set, so the hypothesis stays advice.
func renderNoPriorValue(_ *builder, t target) (rendered, error) {
	return rendered{}, refuse(ReasonInvalidTarget, "the evidence pack does not hold the "+
		"current value %s would replace, so no exact rollback can be written", t)
}

// objectName is prefix + relation + _columns + suffix in lower case,
// within PostgreSQL's identifier limit (a long name keeps a hash of the
// full one, so two long names do not collide).
func objectName(prefix, relation string, columns []string, suffix string) string {
	raw := prefix + relation + "_" + strings.Join(columns, "_") + suffix
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '_'
	}, raw)
	if len(clean) <= maxIdentBytes {
		return clean
	}
	sum := sha256.Sum256([]byte(raw))
	tag := "_" + hex.EncodeToString(sum[:])[:8]
	return clean[:maxIdentBytes-len(tag)-len(suffix)] + tag + suffix
}
