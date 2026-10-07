package tuner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CatalogFacts are the catalog facts the plan heuristics need (Phase 0
// item 11): estimated rows per table and, per table, the valid,
// non-partial btree indexes with a plain leading column. Keys are
// canonical "schema.table". A cycle reads them only for the relations its
// plans name, each name once (perf gate: the whole catalog every cycle cost
// 136 ms at 5,000 tables).
type CatalogFacts struct {
	Tables  map[string]int64       `json:"tables"`
	Indexes map[string][]IndexFact `json:"indexes"`
	// read is the relation names already read this cycle (every schema's
	// relation of that name, so an unqualified name still resolves).
	read map[string]bool
}

// IndexFact is one usable btree index of a table.
type IndexFact struct {
	Name          string `json:"name"`
	LeadingColumn string `json:"leading_column"`
}

// catalogFactsSQL reads the estimated rows and usable btree indexes (valid,
// ready, non-partial, leading key a plain column) of the user tables of
// the given names, in every schema.
const catalogFactsSQL = `/* pg_sage */
SELECT n.nspname || '.' || c.relname, GREATEST(c.reltuples, 0)::bigint,
       COALESCE((SELECT json_agg(json_build_object('name', i.relname,
                     'leading_column', a.attname) ORDER BY i.relname)
                   FROM pg_catalog.pg_index x
                   JOIN pg_catalog.pg_class i ON i.oid = x.indexrelid
                   JOIN pg_catalog.pg_am am ON am.oid = i.relam
                   JOIN pg_catalog.pg_attribute a
                     ON a.attrelid = c.oid AND a.attnum = x.indkey[0]
                  WHERE x.indrelid = c.oid AND x.indisvalid AND x.indisready
                    AND x.indpred IS NULL AND am.amname = 'btree'), '[]')::text
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'm', 'p')
   AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage', 'hint_plan')
   AND n.nspname NOT LIKE 'pg_toast%'
   AND n.nspname NOT LIKE 'pg_temp%'
   AND c.relname = ANY($1::text[])`

// LoadCatalogFacts reads the facts of the relations named relnames. A nil
// pool or no name yields empty facts without a query.
func LoadCatalogFacts(ctx context.Context, pool *pgxpool.Pool,
	relnames []string) (*CatalogFacts, error) {
	facts := newCatalogFacts()
	return facts, facts.ensure(ctx, pool, relnames)
}

func newCatalogFacts() *CatalogFacts {
	return &CatalogFacts{Tables: map[string]int64{}, Indexes: map[string][]IndexFact{},
		read: map[string]bool{}}
}

// ensure reads the facts of the names not read yet; on error nothing is
// marked read.
func (f *CatalogFacts) ensure(ctx context.Context, pool *pgxpool.Pool,
	relnames []string) error {
	var missing []string
	for _, name := range relnames {
		if name != "" && !f.read[name] {
			missing = append(missing, name)
		}
	}
	if pool == nil || len(missing) == 0 {
		return nil
	}
	rows, err := pool.Query(ctx, catalogFactsSQL, missing)
	if err != nil {
		return fmt.Errorf("load catalog facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, raw string
		var reltuples int64
		if err := rows.Scan(&table, &reltuples, &raw); err != nil {
			return fmt.Errorf("scan catalog facts: %w", err)
		}
		var idx []IndexFact
		if err := json.Unmarshal([]byte(raw), &idx); err != nil {
			return fmt.Errorf("decode indexes of %s: %w", table, err)
		}
		f.Tables[table] = reltuples
		if len(idx) > 0 {
			f.Indexes[table] = idx
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate catalog facts: %w", err)
	}
	for _, name := range missing {
		f.read[name] = true
	}
	return nil
}

// resolve returns the facts key of a plan relation. Plain EXPLAIN omits
// the schema, so an unqualified name resolves only when exactly one
// schema has a table of that name.
func (f *CatalogFacts) resolve(schema, rel string) (string, bool) {
	if f == nil || rel == "" {
		return "", false
	}
	if schema != "" {
		key := schema + "." + rel
		_, ok := f.Tables[key]
		return key, ok
	}
	match := ""
	for key := range f.Tables {
		if strings.HasSuffix(key, "."+rel) && !strings.Contains(key[:len(key)-len(rel)-1], ".") {
			if match != "" {
				return "", false
			}
			match = key
		}
	}
	return match, match != ""
}

// TableRows returns the estimated row count of a plan relation.
func (f *CatalogFacts) TableRows(schema, rel string) (int64, bool) {
	key, ok := f.resolve(schema, rel)
	if !ok {
		return 0, false
	}
	return f.Tables[key], true
}

// UsableIndex returns a btree index whose leading column a top-level
// conjunct of filter compares with a btree operator (=, <, <=, >, >=,
// = ANY). Disjunctions and expressions over the column do not qualify.
func (f *CatalogFacts) UsableIndex(schema, rel, filter string) (string, bool) {
	key, ok := f.resolve(schema, rel)
	if !ok {
		return "", false
	}
	cols := filterColumns(filter)
	for _, idx := range f.Indexes[key] {
		if cols[idx.LeadingColumn] {
			return idx.Name, true
		}
	}
	return "", false
}
