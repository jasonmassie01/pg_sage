package impact

import (
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// relation is one touched relation's bounded live context. A read that
// failed leaves its facts unset and its name in failed, so a rule that
// needs them reports unknown instead of none.
type relation struct {
	name         string
	schema, rel  string
	found        bool
	created      bool
	oid          uint32
	relkind      string
	reltuples    float64
	totalBytes   int64
	heapBytes    int64
	columns      map[string]column
	indexes      []index
	constraints  []constraint
	views        []viewDep
	publications []string
	inherits     int
	// homonyms counts the other schemas' tables and views of this name: an
	// unqualified workload statement may mean any of them.
	homonyms int64
	rate     writeRate
	evID     string
	failed   map[string]string
}

type column struct {
	name    string
	typ     string
	notNull bool
}

type index struct {
	name     string
	def      string
	unique   bool
	primary  bool
	valid    bool
	scans    int64
	lastScan *time.Time
	backing  []string
	parsed   analyzer.ParsedIndex
}

type constraint struct {
	name       string
	typ        string
	columns    []string
	refRel     string
	refColumns []string
	validated  bool
	index      string
}

type viewDep struct {
	name    string
	columns []string
}

// Relation reads; their names key relation.failed.
const (
	readSize        = "size"
	readColumns     = "columns"
	readIndexes     = "indexes"
	readConstraints = "constraints"
	readViews       = "dependent views"
	readPubs        = "publications"
	readInherits    = "partitions"
	readRate        = "write rate"
)

// relkindNames labels pg_class.relkind.
var relkindNames = map[string]string{"r": "table", "p": "partitioned", "v": "view",
	"m": "materialized_view", "f": "foreign_table", "i": "index", "S": "sequence"}

func (r *run) readVersion() {
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_version v1 */
		SELECT current_setting('server_version_num')::int`).Scan(&r.version)
	if r.readFailed(err, nil, "server version") {
		r.version = 0
	}
}

// loadRelations resolves every relation name of the statements to its
// schema and loads the context of each.
func (r *run) loadRelations() {
	for _, s := range r.stmts {
		for i := range s.parts {
			r.resolvePart(s, &s.parts[i])
		}
		if s.kind == "select" {
			r.readRelations(s)
		}
		if r.err != nil {
			return
		}
	}
	names := make([]string, 0, len(r.rels))
	for name := range r.rels {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		r.loadRelation(r.rels[name])
		if r.err != nil {
			return
		}
	}
	r.loadWriteRates()
}

// readRelations adds the relations a read statement scans to s.
func (r *run) readRelations(s *stmt) {
	refs, err := sqlast.References(s.sql)
	if err != nil {
		return
	}
	for _, n := range refs.Relations {
		r.resolveName(s, &n)
		r.relationFor(n)
		if q := qualified(n); !hasString(s.relations, q) {
			s.relations = append(s.relations, q)
		}
	}
}

// relationFor registers name and returns its context (not loaded yet).
func (r *run) relationFor(n sqlast.QualifiedName) *relation {
	name := qualified(n)
	if rel, ok := r.rels[name]; ok {
		return rel
	}
	rel := &relation{name: name, schema: n.Schema, rel: n.Name, failed: map[string]string{},
		columns: map[string]column{}}
	r.rels[name] = rel
	return rel
}

// resolveName fills an unqualified name's schema: the relation's schema
// when the catalog has it, otherwise the session's current schema.
func (r *run) resolveName(s *stmt, n *sqlast.QualifiedName) {
	if n.Name == "" || n.Schema != "" {
		return
	}
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_resolve v1 */
		SELECT coalesce((SELECT n.nspname FROM pg_catalog.pg_class c
		  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.oid = pg_catalog.to_regclass($1)), current_schema())`,
		pgx.Identifier{n.Name}.Sanitize()).Scan(&n.Schema)
	r.readFailed(err, s, "schema of "+n.Name)
}

// loadRelation reads one relation: identity first (no table lock), then
// size, columns, indexes, constraints, views, publications, partitions.
func (r *run) loadRelation(rel *relation) {
	var kind string
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_relation v2 */
		SELECT c.oid, c.relkind::text, c.reltuples::float8,
		       (SELECT count(*) FROM pg_catalog.pg_class o
		         WHERE o.relname = c.relname AND o.oid <> c.oid
		           AND o.relkind IN ('r', 'p', 'v', 'm', 'f'))
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, rel.schema, rel.rel).
		Scan(&rel.oid, &kind, &rel.reltuples, &rel.homonyms)
	if errors.Is(err, pgx.ErrNoRows) {
		rel.created = r.created[rel.name]
		return
	}
	if r.readFailed(err, nil, rel.name) {
		rel.failed["relation"] = "read failed"
		return
	}
	rel.found, rel.relkind = true, kind
	for _, load := range []func(*relation){r.loadSize, r.loadColumns, r.loadIndexes,
		r.loadConstraints, r.loadViews, r.loadPublications, r.loadInherits} {
		load(rel)
		if r.err != nil {
			return
		}
	}
}

// relFailed records a failed read of rel (and whether the run stops).
func (r *run) relFailed(err error, rel *relation, what string) bool {
	if err == nil {
		return false
	}
	if r.readFailed(err, nil, what+" of "+rel.name) && r.err == nil {
		reason, _ := classifyReadError(err)
		rel.failed[what] = reason
	}
	return true
}

func (r *run) loadSize(rel *relation) {
	err := r.reader.QueryRow(r.ctx, `/* pg_sage change_review_relation_size v1 */
		SELECT pg_catalog.pg_total_relation_size($1::oid),
		       pg_catalog.pg_relation_size($1::oid)`, rel.oid).
		Scan(&rel.totalBytes, &rel.heapBytes)
	r.relFailed(err, rel, readSize)
}

func (r *run) loadColumns(rel *relation) {
	rows, err := r.reader.Query(r.ctx, `/* pg_sage change_review_columns v1 */
		SELECT a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull
		FROM pg_catalog.pg_attribute a
		WHERE a.attrelid = $1::oid AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`, rel.oid)
	if r.relFailed(err, rel, readColumns) {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.name, &c.typ, &c.notNull); err != nil {
			r.relFailed(err, rel, readColumns)
			return
		}
		rel.columns[c.name] = c
	}
	r.relFailed(rows.Err(), rel, readColumns)
}
