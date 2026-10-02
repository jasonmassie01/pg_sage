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
// canonical "schema.table". Facts are loaded once per tuner cycle and
// read-only afterwards.
type CatalogFacts struct {
	Tables  map[string]int64       `json:"tables"`
	Indexes map[string][]IndexFact `json:"indexes"`
}

// IndexFact is one usable btree index of a table.
type IndexFact struct {
	Name          string `json:"name"`
	LeadingColumn string `json:"leading_column"`
}

// catalogFactsSQL reads every user table's estimated rows and its usable
// btree indexes (valid, ready, non-partial, leading key a plain column).
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
   AND n.nspname NOT LIKE 'pg_temp%'`

// LoadCatalogFacts reads the catalog facts. A nil pool yields empty facts
// (no catalog-dependent symptom can then be detected).
func LoadCatalogFacts(ctx context.Context, pool *pgxpool.Pool) (*CatalogFacts, error) {
	facts := &CatalogFacts{Tables: map[string]int64{}, Indexes: map[string][]IndexFact{}}
	if pool == nil {
		return facts, nil
	}
	rows, err := pool.Query(ctx, catalogFactsSQL)
	if err != nil {
		return nil, fmt.Errorf("load catalog facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, raw string
		var reltuples int64
		if err := rows.Scan(&table, &reltuples, &raw); err != nil {
			return nil, fmt.Errorf("scan catalog facts: %w", err)
		}
		var idx []IndexFact
		if err := json.Unmarshal([]byte(raw), &idx); err != nil {
			return nil, fmt.Errorf("decode indexes of %s: %w", table, err)
		}
		facts.Tables[table] = reltuples
		if len(idx) > 0 {
			facts.Indexes[table] = idx
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate catalog facts: %w", err)
	}
	return facts, nil
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
