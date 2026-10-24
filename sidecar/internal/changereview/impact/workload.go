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
	maxRatedQueryIDs = 200
	hiddenQueryText  = "<insufficient privilege>"
)

// workload is the statements of pg_stat_statements that name a touched
// relation as a whole word, parsed with sqlast.References and joined to
// their query_store history. available is false when pg_stat_statements
// cannot be read (reason says why).
type workload struct {
	available bool
	reason    string
	fix       string
	hidden    int64
	// unqualified counts, per relation, the statements naming it without a
	// schema that were not attributed to it: another schema has a
	// relation of that name.
	unqualified   map[string]int
	stmts         []*workStmt
	byFingerprint map[string]int64
	byRelation    map[string][]*workStmt
}

// workStmt is one workload statement.
type workStmt struct {
	id          int64
	text        string
	calls       int64
	fingerprint string
	refs        sqlast.Refs
	tags        map[string]string
	rate        queryRate
	rateKnown   bool
	evID        string
}

// lockMode is the strongest table lock the statement takes.
func (w *workStmt) lockMode() sqlast.LockMode { return w.refs.LockMode }

// loadWorkload is stage 3 over every relation the run knows.
func (r *run) loadWorkload() {
	var names, quals []string
	for _, rel := range r.rels {
		names = append(names, rel.rel)
		if rel.schema != "" {
			quals = append(quals, rel.schema+"."+rel.rel)
		}
	}
	r.work = r.buildWorkload(names, quals)
}

func (r *run) buildWorkload(names, quals []string) *workload {
	w := &workload{byFingerprint: map[string]int64{}, byRelation: map[string][]*workStmt{},
		unqualified: map[string]int{}}
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
	if err := r.readStatements(w, schema, names, quals); err != nil {
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

// loadWorkloadFacts is stage 3 with its unknowns recorded.
func (r *run) loadWorkloadFacts() {
	r.loadWorkload()
	if r.work != nil {
		r.unqualifiedFacts()
	}
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
// Statements naming a relation schema-qualified come first, so unqualified
// ones (another tenant's, perhaps) cannot crowd them out of the bound.
func (r *run) readStatements(w *workload, schema string, names, quals []string) error {
	key := schema + "\x00" + wordPattern(names) + "\x00" + qualifiedPattern(quals)
	read, ok := sessionCacheOf(r.ctx).workload(key)
	if !ok {
		var err error
		if read, err = r.fetchStatements(schema, names, quals); err != nil {
			return err
		}
		sessionCacheOf(r.ctx).keepWorkload(key, read)
	}
	w.hidden = read.hidden
	for _, row := range read.rows {
		ws := row
		r.indexStatement(w, &ws)
	}
	return nil
}

// fetchStatements reads the matching statements and the hidden count.
func (r *run) fetchStatements(schema string, names, quals []string) (workloadRead, error) {
	var read workloadRead
	view := pgx.Identifier{schema, "pg_stat_statements"}.Sanitize()
	rows, err := r.reader.Query(r.ctx, fmt.Sprintf(`/* pg_sage change_review_workload v2 */
		SELECT s.queryid, min(s.query), sum(s.calls)::int8
		FROM %s s
		WHERE s.dbid = (SELECT oid FROM pg_catalog.pg_database
		                WHERE datname = current_database())
		  AND s.queryid IS NOT NULL AND s.query ~* $1 AND %s
		GROUP BY s.queryid ORDER BY bool_or(s.query ~* $2) DESC, 3 DESC, 1 LIMIT %d`, view,
		appworkload.AdviceSQL("s.query"), maxWorkloadRows), wordPattern(names),
		qualifiedPattern(quals))
	if err != nil {
		return read, err
	}
	read.rows, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (workStmt, error) {
		var ws workStmt
		return ws, row.Scan(&ws.id, &ws.text, &ws.calls)
	})
	if err != nil {
		return read, err
	}
	err = r.reader.QueryRow(r.ctx, fmt.Sprintf(`/* pg_sage change_review_hidden v1 */
		SELECT count(*) FROM %s s
		WHERE s.dbid = (SELECT oid FROM pg_catalog.pg_database
		                WHERE datname = current_database())
		  AND s.query = $1`, view), hiddenQueryText).Scan(&read.hidden)
	return read, err
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
		rel, ambiguous := r.matchRelation(ref)
		if ambiguous != nil {
			w.unqualified[ambiguous.name]++
		}
		if rel != nil {
			w.byRelation[rel.name] = append(w.byRelation[rel.name], ws)
			matched = true
		}
	}
	if !matched {
		return
	}
	w.stmts = append(w.stmts, ws)
	if fp, err := sqlast.Fingerprint(ws.text); err == nil {
		ws.fingerprint = fp
		if _, dup := w.byFingerprint[fp]; !dup {
			w.byFingerprint[fp] = ws.id
		}
	}
}

// matchRelation is the known relation ref names: the same schema, or for
// a schema-less reference the only relation of that name in the
// database. A schema-less reference to a name several schemas hold (a
// schema-per-tenant database, sessions with their own search_path) is
// not attributed: ambiguous is then the known relation it may mean.
func (r *run) matchRelation(ref sqlast.QualifiedName) (hit, ambiguous *relation) {
	if ref.Schema != "" {
		return r.rels[qualified(ref)], nil
	}
	for _, rel := range r.rels {
		if rel.rel != ref.Name {
			continue
		}
		if hit != nil || (rel.found && rel.homonyms > 0) {
			return nil, rel
		}
		hit = rel
	}
	return hit, nil
}

// unqualifiedFacts is CR-17 for the relations whose unqualified workload
// statements could not be attributed.
func (r *run) unqualifiedFacts() {
	names := make([]string, 0, len(r.work.unqualified))
	for name := range r.work.unqualified {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		n, rel := r.work.unqualified[name], r.rels[name]
		if rel == nil {
			continue
		}
		r.unknownFact(nil, "unqualified statements naming "+rel.rel, "schema unknown",
			fmt.Sprintf("%d workload statements name %s without a schema, and %d other "+
				"tables or views of that name exist; which one each reads depends on its "+
				"session's search_path, so they are not counted for %s", n, rel.rel,
				rel.homonyms, name), "qualify the table names the application sends")
	}
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
	cache := sessionCacheOf(r.ctx)
	rates, missing := cache.rates(ids)
	if len(missing) > 0 {
		read, err := r.queryRates(missing)
		if r.readFailed(err, nil, "query_store rates") {
			return
		}
		cache.keepRates(missing, read)
		for id, q := range read {
			rates[id] = q
		}
	}
	for _, ws := range w.stmts {
		if q, ok := rates[ws.id]; ok {
			ws.rate, ws.rateKnown = q, true
		}
	}
}

var regexSpecial = regexp.MustCompile(`[.^$|?*+()\[\]{}\\]`)

// qualifiedPattern matches any of quals ("schema.name") as SQL writes it,
// plain or double-quoted (PostgreSQL regex); with none, a pattern that
// matches nothing.
func qualifiedPattern(quals []string) string {
	var alts []string
	for _, q := range quals {
		schema, name, ok := strings.Cut(q, ".")
		if !ok {
			continue
		}
		s := regexSpecial.ReplaceAllString(schema, `\$0`)
		n := regexSpecial.ReplaceAllString(name, `\$0`)
		alts = append(alts, s+`\.`+n, `"`+s+`"\."`+n+`"`)
	}
	if len(alts) == 0 {
		return `$.^`
	}
	sort.Strings(alts)
	return `(^|[^a-z0-9_$"])(` + strings.Join(alts, "|") + `)([^a-z0-9_$]|$)`
}

// wordPattern matches any of names as a whole word (PostgreSQL regex).
func wordPattern(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, regexSpecial.ReplaceAllString(n, `\$0`))
	}
	sort.Strings(quoted)
	return `\m(` + strings.Join(quoted, "|") + `)\M`
}
