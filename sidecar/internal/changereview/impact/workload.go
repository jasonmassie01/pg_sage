package impact

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/agenttools"
	"github.com/pg-sage/sidecar/internal/sqlast"
	appworkload "github.com/pg-sage/sidecar/internal/workload"
)

// Workload index bounds.
const (
	maxWorkloadRows  = 2000
	maxRatedQueryIDs = 500
	hiddenQueryText  = "<insufficient privilege>"
)

// workload is the statements of pg_stat_statements that name a touched
// relation as a whole word, parsed with sqlast.References and joined to
// their query_store history. available is false when pg_stat_statements
// cannot be read (reason says why).
type workload struct {
	available     bool
	reason        string
	fix           string
	hidden        int64
	stmts         []*workStmt
	byFingerprint map[string]int64
	byRelation    map[string][]*workStmt
}

// workStmt is one workload statement.
type workStmt struct {
	id        int64
	text      string
	calls     int64
	refs      sqlast.Refs
	tags      map[string]string
	rate      queryRate
	rateKnown bool
	evID      string
}

// lockMode is the strongest table lock the statement takes.
func (w *workStmt) lockMode() sqlast.LockMode { return w.refs.LockMode }

// loadWorkload is stage 3 over every relation the run knows.
func (r *run) loadWorkload() {
	var names []string
	for _, rel := range r.rels {
		names = append(names, rel.rel)
	}
	r.work = r.buildWorkload(names)
}

func (r *run) buildWorkload(names []string) *workload {
	w := &workload{byFingerprint: map[string]int64{}, byRelation: map[string][]*workStmt{}}
	if len(names) == 0 {
		w.available = true
		return w
	}
	schema, err := r.statementsSchema()
	if err != nil || schema == "" {
		w.reason, w.fix = "pg_stat_statements is not installed", "CREATE EXTENSION "+
			"pg_stat_statements (and preload it) in this database"
		if err != nil {
			w.reason, w.fix = classifyReadError(err)
			r.readFailed(err, nil, "pg_stat_statements")
		}
		return w
	}
	if err := r.readStatements(w, schema, names); err != nil {
		w.reason, w.fix = classifyReadError(err)
		if !r.readFailed(err, nil, "pg_stat_statements") || r.err != nil {
			return w
		}
		return w
	}
	w.available = true
	r.joinRates(w)
	return w
}

// statementsSchema is the schema pg_stat_statements lives in ("" when it
// is not installed).
func (r *run) statementsSchema() (string, error) {
	var schema string
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_pgss v1 */
		SELECT n.nspname FROM pg_catalog.pg_extension e
		JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
		WHERE e.extname = 'pg_stat_statements'`).Scan(&schema)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return schema, err
}

// readStatements reads the matching statements and the hidden count.
func (r *run) readStatements(w *workload, schema string, names []string) error {
	view := pgx.Identifier{schema, "pg_stat_statements"}.Sanitize()
	rows, err := r.reader.Query(r.ctx, fmt.Sprintf(`/* pg_sage change_review_workload v1 */
		SELECT s.queryid, min(s.query), sum(s.calls)::int8
		FROM %s s
		WHERE s.dbid = (SELECT oid FROM pg_catalog.pg_database
		                WHERE datname = current_database())
		  AND s.queryid IS NOT NULL AND s.query ~* $1 AND %s
		GROUP BY s.queryid ORDER BY 3 DESC, 1 LIMIT %d`, view, appworkload.AdviceSQL("s.query"),
		maxWorkloadRows), wordPattern(names))
	if err != nil {
		return err
	}
	texts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*workStmt, error) {
		ws := &workStmt{}
		return ws, row.Scan(&ws.id, &ws.text, &ws.calls)
	})
	if err != nil {
		return err
	}
	err = r.reader.QueryRow(r.ctx, fmt.Sprintf(`/* pg_sage change_review_hidden v1 */
		SELECT count(*) FROM %s s
		WHERE s.dbid = (SELECT oid FROM pg_catalog.pg_database
		                WHERE datname = current_database())
		  AND s.query = $1`, view), hiddenQueryText).Scan(&w.hidden)
	if err != nil {
		return err
	}
	for _, ws := range texts {
		r.indexStatement(w, ws)
	}
	return nil
}

// indexStatement parses a workload statement and files it under every
// known relation it references. Statements that are not DML are skipped.
func (r *run) indexStatement(w *workload, ws *workStmt) {
	refs, err := sqlast.References(ws.text)
	if err != nil || refs.Kind == sqlast.StmtOther {
		return
	}
	ws.refs = refs
	ws.tags = agenttools.ParseSQLCommenter(ws.text)
	matched := false
	for _, ref := range refs.Relations {
		if rel := r.matchRelation(ref); rel != nil {
			w.byRelation[rel.name] = append(w.byRelation[rel.name], ws)
			matched = true
		}
	}
	if !matched {
		return
	}
	w.stmts = append(w.stmts, ws)
	if fp, err := sqlast.Fingerprint(ws.text); err == nil {
		if _, dup := w.byFingerprint[fp]; !dup {
			w.byFingerprint[fp] = ws.id
		}
	}
}

// matchRelation is the known relation ref names: the same schema, or for
// a schema-less reference the only known relation of that name.
func (r *run) matchRelation(ref sqlast.QualifiedName) *relation {
	if ref.Schema != "" {
		return r.rels[qualified(ref)]
	}
	var hit *relation
	for _, rel := range r.rels {
		if rel.rel == ref.Name {
			if hit != nil {
				return nil
			}
			hit = rel
		}
	}
	return hit
}

// joinRates adds 7-day calls and peak rates from sage.query_store.
func (r *run) joinRates(w *workload) {
	sorted := append([]*workStmt(nil), w.stmts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].calls > sorted[j].calls })
	ids := make([]int64, 0, min(len(sorted), maxRatedQueryIDs))
	for _, ws := range sorted[:min(len(sorted), maxRatedQueryIDs)] {
		ids = append(ids, ws.id)
	}
	if len(ids) == 0 {
		return
	}
	rates, err := r.queryRates(ids)
	if r.readFailed(err, nil, "query_store rates") {
		return
	}
	for _, ws := range w.stmts {
		if q, ok := rates[ws.id]; ok {
			ws.rate, ws.rateKnown = q, true
		}
	}
}

var regexSpecial = regexp.MustCompile(`[.^$|?*+()\[\]{}\\]`)

// wordPattern matches any of names as a whole word (PostgreSQL regex).
func wordPattern(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, regexSpecial.ReplaceAllString(n, `\$0`))
	}
	sort.Strings(quoted)
	return `\m(` + strings.Join(quoted, "|") + `)\M`
}
