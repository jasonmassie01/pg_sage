package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// oldIndexInfo is the catalog's view of the index a replacement drops.
type oldIndexInfo struct {
	OID         int64
	Definition  string
	Constrained bool // unique, primary, exclusion, or behind a constraint
	OnTable     bool
	Lead        []string // key column names ("" for an expression)
	Partial     bool
}

const oldIndexSQL = `/* pg_sage */ SELECT i.indexrelid::bigint,
	pg_get_indexdef(i.indexrelid),
	i.indisunique OR i.indisprimary OR i.indisexclusion OR EXISTS (
	    SELECT 1 FROM pg_constraint c WHERE c.conindid = i.indexrelid),
	i.indrelid = to_regclass($2), i.indpred IS NOT NULL,
	ARRAY(SELECT COALESCE(a.attname::text, '') FROM unnest((i.indkey::smallint[])[0:i.indnkeyatts - 1])
	      WITH ORDINALITY k(attnum, n)
	      LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
	      ORDER BY k.n)
	FROM pg_index i WHERE i.indexrelid = to_regclass($1)`

// readOldIndex reads the old index; a missing one is an identity change.
func (e *Executor) readOldIndex(ctx context.Context, p IndexReplace) (oldIndexInfo, error) {
	var o oldIndexInfo
	err := e.pool.QueryRow(ctx, oldIndexSQL, p.OldIndex, p.Table).Scan(&o.OID,
		&o.Definition, &o.Constrained, &o.OnTable, &o.Partial, &o.Lead)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, fmt.Errorf("%w: %s no longer exists", ErrReplaceIdentityChanged, p.OldIndex)
	}
	if err != nil {
		return o, fmt.Errorf("read %s: %w", p.OldIndex, err)
	}
	return o, nil
}

// checkOldIndex refuses an old index that backs a constraint (checked
// first: it is never replaced), is not the proposed one (OID and
// definition), is not on the table, or is not subsumed by the new one.
func (e *Executor) checkOldIndex(ctx context.Context, p IndexReplace, oid int64,
	definition string) (oldIndexInfo, error) {
	o, err := e.readOldIndex(ctx, p)
	if err != nil {
		return o, err
	}
	if o.Constrained {
		return o, fmt.Errorf("%w: %s", ErrReplaceConstraintBacked, p.OldIndex)
	}
	if oid <= 0 || o.OID != oid || !sameDefinition(o.Definition, definition) ||
		!o.OnTable || !sameDefinition(o.Definition, p.RecreateSQL) {
		return o, fmt.Errorf("%w: %s is OID %d %q, proposed OID %d %q",
			ErrReplaceIdentityChanged, p.OldIndex, o.OID, o.Definition, oid, definition)
	}
	return o, p.checkSubsumes(o.Definition)
}

// sameDefinition compares index definitions ignoring CONCURRENTLY,
// whitespace and a trailing semicolon.
func sameDefinition(a, b string) bool {
	norm := func(s string) string {
		f := strings.Fields(strings.TrimRight(strings.TrimSpace(s), ";"))
		out := f[:0]
		for _, w := range f {
			if !strings.EqualFold(w, "CONCURRENTLY") {
				out = append(out, w)
			}
		}
		return strings.Join(out, " ")
	}
	return a != "" && norm(a) == norm(b)
}

// checkForeignKeys refuses a replacement that would leave a foreign key
// the old index supports without a supporting index.
func (e *Executor) checkForeignKeys(ctx context.Context, p IndexReplace,
	old oldIndexInfo) error {
	rows, err := e.pool.Query(ctx, `/* pg_sage */ SELECT c.conname,
		ARRAY(SELECT a.attname::text FROM unnest(c.conkey) k(attnum)
		      JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum)
		FROM pg_constraint c WHERE c.contype = 'f' AND c.conrelid = to_regclass($1)`,
		p.Table)
	if err != nil {
		return fmt.Errorf("read foreign keys of %s: %w", p.Table, err)
	}
	defer rows.Close()
	newLead, newPartial := newIndexLead(p.CreateSQL)
	for rows.Next() {
		var name string
		var cols []string
		if err := rows.Scan(&name, &cols); err != nil {
			return fmt.Errorf("scan foreign key: %w", err)
		}
		if supportsForeignKey(old.Lead, old.Partial, cols) &&
			!supportsForeignKey(newLead, newPartial, cols) {
			return fmt.Errorf("%w: %s on (%s)", ErrReplaceForeignKey, name,
				strings.Join(cols, ", "))
		}
	}
	return rows.Err()
}

// newIndexLead is the new index's key column names in order (expressions
// are "") and whether it is partial.
func newIndexLead(create string) ([]string, bool) {
	spec, err := optimizer.ParseIndexDDL(create)
	if err != nil {
		return nil, false
	}
	var lead []string
	for _, key := range splitTopLevel(spec.Keys) {
		fields := strings.Fields(strings.TrimSpace(key))
		name := ""
		if len(fields) > 0 && !strings.ContainsAny(fields[0], "()") {
			name = unquoteIdentifier(fields[0])
		}
		lead = append(lead, name)
	}
	return lead, spec.Where != ""
}

// replaceCatalogView reads both indexes now.
func (e *Executor) replaceCatalogView(ctx context.Context, p IndexReplace) (
	replaceCatalog, error) {
	var c replaceCatalog
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT
		n.indexrelid IS NOT NULL, COALESCE(n.indisvalid AND n.indisready, false),
		o.indexrelid IS NOT NULL, COALESCE(o.indisvalid AND o.indisready, false)
		FROM (SELECT 1) x
		LEFT JOIN pg_index n ON n.indexrelid = to_regclass($1)
		LEFT JOIN pg_index o ON o.indexrelid = to_regclass($2)`,
		p.NewIndex, p.OldIndex).Scan(&c.NewExists, &c.NewValid, &c.OldExists, &c.OldValid)
	if err != nil {
		return c, fmt.Errorf("read the indexes of %s: %w", p.Table, err)
	}
	return c, nil
}

// indexOID is an index's OID (0 when absent).
func (e *Executor) indexOID(ctx context.Context, qualified string) (int64, error) {
	var oid int64
	err := e.pool.QueryRow(ctx, `/* pg_sage */ SELECT COALESCE(to_regclass($1)::oid::bigint,
		0)`, qualified).Scan(&oid)
	if err != nil {
		return 0, fmt.Errorf("look up %s: %w", qualified, err)
	}
	return oid, nil
}
