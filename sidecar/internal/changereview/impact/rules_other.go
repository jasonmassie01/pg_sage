package impact

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// Statements of kind other: the ones the engine judges (TRUNCATE, DROP
// SCHEMA, LOCK TABLE through CR-02) and, for every other kind it has no
// rule for, a CR-17 warning. Unknown is never none: a statement pg_sage
// cannot analyze is never part of a safe review.

// benignCommands are statement kinds that change no existing object's
// data, locks or behavior (or are reads, planned by CR-14).
var benignCommands = map[string]bool{"SelectStmt": true, "CommentStmt": true,
	"ExplainStmt": true, "VariableShowStmt": true, "CreateSchemaStmt": true,
	"CreateSeqStmt": true, "CreateEnumStmt": true, "CompositeTypeStmt": true,
	"CreateDomainStmt": true}

// judgedCommands have rules of their own (CR-01, CR-02, CR-04, CR-05).
var judgedCommands = map[string]bool{"TruncateStmt": true, "LockStmt": true}

// benignSubcommands are ALTER TABLE subcommands that rewrite nothing and
// change no data or behavior the workload sees (CR-02 still prices their
// ACCESS EXCLUSIVE lock).
var benignSubcommands = map[string]bool{"AT_SetStatistics": true, "AT_SetOptions": true,
	"AT_ResetOptions": true, "AT_SetRelOptions": true, "AT_ResetRelOptions": true,
	"AT_SetStorage": true, "AT_SetCompression": true, "AT_ClusterOn": true,
	"AT_DropCluster": true}

// unmodeledSubject names a part the engine has no rule for ("" when it
// has one, or needs none).
func unmodeledSubject(p sqlast.DDLStatement) string {
	if p.Kind != sqlast.DDLOther || benignCommands[p.Command] || judgedCommands[p.Command] {
		return ""
	}
	switch p.Command {
	case "AlterTableStmt":
		if benignSubcommands[p.Subcommand] {
			return ""
		}
		return "ALTER TABLE " + strings.TrimPrefix(p.Subcommand, "AT_")
	case "DropStmt":
		if p.ObjectType == "schema" {
			return ""
		}
		return "DROP " + strings.ToUpper(strings.ReplaceAll(p.ObjectType, "_", " "))
	case "VacuumStmt":
		if t := strings.ToUpper(p.Text); strings.HasPrefix(t, "ANALYZE") ||
			strings.HasPrefix(t, "ANALYSE") {
			return ""
		}
	}
	return commandKind(p.Command) + " statement"
}

// unmodeled is CR-17 for the parts of s no rule covers. A statement the
// classifier already scored (CR-01: VACUUM FULL, CLUSTER, REINDEX, ...)
// is covered.
func (r *run) unmodeled(s *stmt, scored bool) {
	if scored {
		return
	}
	for _, p := range s.parts {
		subject := unmodeledSubject(p)
		if subject == "" {
			continue
		}
		detail := "pg_sage has no rule for this kind of statement, so its locks, " +
			"rewrites and effect on the workload were not analyzed"
		if p.Behavior == "cascade" {
			detail += "; with CASCADE every object depending on it is dropped too"
		}
		r.unknownFact(s, subject, "not modeled", detail, "review it by hand")
	}
}

// otherRules judges a part of kind other on a relation the catalog holds.
func (r *run) otherRules(s *stmt, i int, rel *relation) {
	if s.parts[i].Command == "TruncateStmt" {
		r.truncate(s, i, rel)
	}
}

// truncate is CR-01 for TRUNCATE: every row is removed under ACCESS
// EXCLUSIVE, which is data loss on a table with rows.
func (r *run) truncate(s *stmt, i int, rel *relation) {
	p := s.parts[i]
	sev := changereview.SeverityWarning
	rows := "no rows by its statistics"
	if rel.reltuples > 0 {
		sev, rows = changereview.SeverityCritical, fmt.Sprintf("about %.0f rows",
			rel.reltuples)
	} else if rel.reltuples < 0 {
		rows = "an unknown number of rows (never analyzed)"
	}
	text := fmt.Sprintf("TRUNCATE removes every row of %s (%s) under ACCESS EXCLUSIVE; "+
		"nothing can bring them back short of a restore.", rel.name, rows)
	if p.Behavior == "cascade" {
		text += " CASCADE also truncates every table with a foreign key to it."
	}
	r.find(RuleLockAndRewrite, sev, "TRUNCATE removes every row of "+rel.name, text,
		s.evID, rel.evID)
}

// otherObject judges a part of kind other that names no relation: DROP
// SCHEMA.
func (r *run) otherObject(s *stmt, i int) {
	p := s.parts[i]
	if p.Command == "DropStmt" && p.ObjectType == "schema" && p.Object.Name != "" {
		r.dropSchema(s, p)
	}
}

// dropSchema is CR-04 for DROP SCHEMA ... CASCADE of a schema holding
// relations, CR-05 for DROP SCHEMA without CASCADE (it fails), CR-17 for
// a schema that is missing or holds no relation (its other objects are
// not checked).
func (r *run) dropSchema(s *stmt, p sqlast.DDLStatement) {
	schema := p.Object.Name
	found, n, names, err := r.schemaRelations(schema)
	if r.readFailed(err, s, "relations of schema "+schema) {
		return
	}
	switch {
	case !found && !p.IfExists:
		r.unknownFact(s, "schema "+schema, "not found", "the catalog has no such schema, "+
			"so the DROP fails", "")
	case !found:
	case n == 0:
		r.unknownFact(s, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize(), "not modeled",
			"the schema holds no table or view; its functions, types and other objects "+
				"were not checked", "review it by hand")
	case p.Behavior == "cascade":
		id := r.addEvidence("schema", schema, fmt.Sprintf("schema %s: %d relations",
			schema, n), schemaData{Schema: schema, Relations: n, Sample: names})
		r.find(RuleRelationReferenced, changereview.SeverityCritical, "DROP SCHEMA "+schema+
			" CASCADE drops its "+fmt.Sprint(n)+" relations", fmt.Sprintf("DROP SCHEMA %s "+
			"CASCADE drops every object in it: %d tables and views (%s) with their data, "+
			"and everything that depends on them.", schema, n, strings.Join(names, ", ")),
			s.evID, id)
	default:
		id := r.addEvidence("schema", schema, fmt.Sprintf("schema %s: %d relations",
			schema, n), schemaData{Schema: schema, Relations: n, Sample: names})
		r.find(RuleWillFail, changereview.SeverityCritical, "DROP SCHEMA "+schema+
			" will fail", fmt.Sprintf("PostgreSQL refuses to drop schema %s without "+
			"CASCADE: it holds %d tables and views (%s).", schema, n,
			strings.Join(names, ", ")), s.evID, id)
	}
}

// maxSchemaSample bounds the relation names a DROP SCHEMA finding lists.
const maxSchemaSample = 10

// schemaRelations is whether schema exists, how many tables and views it
// holds and the first of their names.
func (r *run) schemaRelations(schema string) (bool, int64, []string, error) {
	var n int64
	var names []string
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_schema_relations v1 */
		SELECT count(c.oid), coalesce((array_agg(n.nspname || '.' || c.relname
		         ORDER BY c.relname) FILTER (WHERE c.oid IS NOT NULL))[1:$2], '{}')
		FROM pg_catalog.pg_namespace n
		LEFT JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid
		 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		WHERE n.nspname = $1 GROUP BY n.oid`, schema, maxSchemaSample).Scan(&n, &names)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, nil, nil
	}
	return err == nil, n, names, err
}
