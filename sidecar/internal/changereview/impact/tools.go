package impact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/optimizer"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// The reasoner's closed, read-only tools queries_touching, table_context
// and whatif_index are backed by the engine; inspect_statement is
// InspectStatement. Every result is citable evidence.

// Tool bounds.
const (
	maxTouching         = 50
	MaxWhatIfStatements = 10
)

var relationArg = regexp.MustCompile(
	`^[A-Za-z_][A-Za-z0-9_$]{0,62}(\.[A-Za-z_][A-Za-z0-9_$]{0,62})?$`)

// Touching is the workload statements using a relation (or one of its
// columns): queryid evidence, busiest first, at most 50. Known is false
// when pg_stat_statements cannot be read or hides statement text; Reason
// says why and Evidence then holds the "unknown:" evidence.
type Touching struct {
	Relation string
	Known    bool
	Reason   string
	Evidence []changereview.Evidence
}

// QueriesTouching is the queries_touching tool. relation is "schema.name"
// or a name the search path resolves (unquoted, folded to lower case);
// column, when set, keeps the statements that use it, read every column
// (SELECT *) or name it ambiguously. A malformed name is ErrInvalid, a
// relation the catalog does not hold ErrNotFound.
func (e *Engine) QueriesTouching(ctx context.Context, relation, column string) (
	Touching, error) {
	r, rel, err := e.toolRelation(ctx, relation)
	if err != nil {
		return Touching{}, err
	}
	r.loadWorkload()
	if r.err != nil {
		return Touching{}, fmt.Errorf("queries touching %s: %w", rel.name, r.err)
	}
	subject := rel.name
	if column != "" {
		subject += "." + column
	}
	t := Touching{Relation: rel.name, Known: r.workloadKnown(nil, subject)}
	if !t.Known {
		t.Reason = r.unknownReason()
		for _, ev := range r.ev {
			if ev.Kind == "unknown" {
				t.Evidence = append(t.Evidence, ev)
			}
		}
		return t, nil
	}
	var stmts []*workStmt
	for _, ws := range r.work.byRelation[rel.name] {
		if column == "" || hasString(columnsOf(ws.refs, rel), column) ||
			readsStar(ws.refs, rel) || ambiguousOn(ws.refs, rel, column) {
			stmts = append(stmts, ws)
		}
	}
	sort.SliceStable(stmts, func(i, j int) bool { return stmts[i].calls > stmts[j].calls })
	for _, ws := range stmts[:min(len(stmts), maxTouching)] {
		r.queryEvidence(ws, rel)
		t.Evidence = append(t.Evidence, r.ev[r.evIdx[ws.evID]])
	}
	return t, nil
}

// unknownReason is the reason of the run's last unknown fact.
func (r *run) unknownReason() string {
	if !r.work.available {
		return r.work.reason
	}
	return "query text hidden"
}

// TableContext is the table_context tool: relation's live context as
// evidence (the table, its indexes with scans and plan users, dependent
// views, publications).
func (e *Engine) TableContext(ctx context.Context, relation string) (
	[]changereview.Evidence, error) {
	r, rel, err := e.toolRelation(ctx, relation)
	if err != nil {
		return nil, err
	}
	r.loadWriteRates()
	r.loadWorkload()
	if r.err != nil {
		return nil, fmt.Errorf("table context of %s: %w", rel.name, r.err)
	}
	r.tableEvidence()
	ids := []string{rel.evID}
	for _, ix := range rel.indexes {
		d := r.indexEvidence(rel, ix)
		ids = append(ids, r.addEvidence("index", d.Index, fmt.Sprintf("%s: %d scans",
			d.Index, ix.scans), d))
	}
	for _, v := range rel.views {
		ids = append(ids, r.addEvidence("view", v.name, v.name+" depends on "+rel.name,
			viewData{View: v.name, Relation: rel.name, Columns: v.columns}))
	}
	for _, p := range rel.publications {
		ids = append(ids, r.addEvidence("publication", p, p+" publishes "+rel.name,
			publicationData{Publication: p, Relation: rel.name}))
	}
	return r.evidenceByID(ids), nil
}

func (r *run) evidenceByID(ids []string) []changereview.Evidence {
	out := make([]changereview.Evidence, 0, len(ids))
	for _, id := range ids {
		if i, ok := r.evIdx[id]; ok {
			out = append(out, r.ev[i])
		}
	}
	return out
}

// toolRelation validates and loads one relation for a tool.
func (e *Engine) toolRelation(ctx context.Context, relation string) (*run, *relation,
	error) {
	relation = strings.TrimSpace(relation)
	if !relationArg.MatchString(relation) {
		return nil, nil, fmt.Errorf("%w: relation %q is not [schema.]name",
			changereview.ErrInvalid, relation)
	}
	n := sqlast.QualifiedName{Name: strings.ToLower(relation)}
	if schema, name, ok := strings.Cut(n.Name, "."); ok {
		n = sqlast.QualifiedName{Schema: schema, Name: name}
	}
	r := e.newRun(ctx, e.d.Config, changereview.Input{})
	r.readVersion()
	r.resolveToolName(&n)
	rel := r.relationFor(n)
	r.loadRelation(rel)
	switch {
	case r.err != nil:
		return nil, nil, fmt.Errorf("load %s: %w", rel.name, r.err)
	case !rel.found:
		return nil, nil, fmt.Errorf("%w: relation %s", changereview.ErrNotFound, rel.name)
	}
	return r, rel, nil
}

// WhatIfIndex is the whatif_index tool: ddl (one CREATE INDEX) measured
// with session-local HypoPG over statements (at most 10, the extracted
// statements the model cites), or over the workload reads of its table
// when none are given. Its "whatif:" evidence carries improvement_pct
// only when something was measured.
func (e *Engine) WhatIfIndex(ctx context.Context, ddl string, statements []string) (
	changereview.Evidence, error) {
	if err := validWhatIf(statements); err != nil {
		return changereview.Evidence{}, err
	}
	if !astAvailable() {
		return changereview.Evidence{}, fmt.Errorf("%w: the SQL parser is not compiled in",
			changereview.ErrUnavailable)
	}
	parts, err := inspectDDL(ddl)
	if err != nil || countStatements(parts) != 1 || parts[0].Kind != sqlast.DDLCreateIndex {
		return changereview.Evidence{}, fmt.Errorf("%w: ddl must be one CREATE INDEX",
			changereview.ErrInvalid)
	}
	r, rel, err := e.toolRelation(ctx, qualified(parts[0].Relation))
	if err != nil {
		return changereview.Evidence{}, err
	}
	r.loadWriteRates()
	r.loadWorkload()
	queries, qids := r.whatIfQueries(rel)
	if len(statements) > 0 {
		queries, qids = givenQueries(statements), nil
	}
	d := r.measureIndex(rel, parts[0].Index, queries, qids, nil)
	if r.err != nil {
		return changereview.Evidence{}, fmt.Errorf("what-if index: %w", r.err)
	}
	sum := sha256.Sum256([]byte(ddl + "\x00" + strings.Join(statements, "\x00")))
	id := r.addEvidence("whatif", "tool."+hex.EncodeToString(sum[:6]), whatIfTitle(d), d)
	return r.ev[r.evIdx[id]], nil
}

func validWhatIf(statements []string) error {
	if len(statements) > MaxWhatIfStatements {
		return fmt.Errorf("%w: at most %d statements, got %d", changereview.ErrInvalid,
			MaxWhatIfStatements, len(statements))
	}
	for _, s := range statements {
		if strings.TrimSpace(s) == "" || len(s) > sqlast.MaxInputBytes {
			return fmt.Errorf("%w: a statement is empty or over %d bytes",
				changereview.ErrInvalid, sqlast.MaxInputBytes)
		}
	}
	return nil
}

// givenQueries is statements as what-if queries with ids 1..n.
func givenQueries(statements []string) []optimizer.QueryInfo {
	out := make([]optimizer.QueryInfo, len(statements))
	for i, s := range statements {
		out[i] = optimizer.QueryInfo{QueryID: int64(i + 1), Text: s, Calls: 1}
	}
	return out
}

// resolveToolName fills an unqualified tool argument's schema: the search
// path's relation, else the only relation of that name in a user schema
// (a model names tables as the code does), else the current schema.
func (r *run) resolveToolName(n *sqlast.QualifiedName) {
	if n.Schema != "" {
		return
	}
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_tool_relation v1 */
		SELECT coalesce(
		  (SELECT n.nspname FROM pg_catalog.pg_class c
		     JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		    WHERE c.oid = pg_catalog.to_regclass($1)),
		  (SELECT min(n.nspname) FROM pg_catalog.pg_class c
		     JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		    WHERE c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		      AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
		      AND n.nspname !~ '^pg_'
		   HAVING count(*) = 1),
		  current_schema())`, pgx.Identifier{n.Name}.Sanitize(), n.Name).Scan(&n.Schema)
	r.readFailed(err, nil, "schema of "+n.Name)
}
