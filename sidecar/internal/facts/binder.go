package facts

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/policy"
)

// ConfirmedSource returns the confirmed, unexpired facts of a database.
type ConfirmedSource interface {
	Confirmed(context.Context) ([]Fact, error)
}

// IndexResolver completes references from the catalog: a relation of
// unknown kind that is an index, and an index without its table, get
// their kind and table.
type IndexResolver interface {
	ResolveIndexes(context.Context, []ObjectRef) ([]ObjectRef, error)
}

// Binder is the policy gate's view of the confirmed facts
// (policy.FactBinder): typed matching of a request's objects against the
// confirmed fact subjects, never model text.
type Binder struct {
	source   ConfirmedSource
	resolver IndexResolver
	now      func() time.Time
}

// NewBinder binds requests with the facts from source; resolver may be nil
// (references are then matched as written); now defaults to time.Now.
func NewBinder(source ConfirmedSource, resolver IndexResolver, now func() time.Time) *Binder {
	if now == nil {
		now = time.Now
	}
	return &Binder{source: source, resolver: resolver, now: now}
}

// Bind implements policy.FactBinder.
func (b *Binder) Bind(ctx context.Context, req policy.ActionRequest) (
	[]policy.FactBinding, error) {
	facts, err := b.source.Confirmed(ctx)
	if err != nil {
		return nil, fmt.Errorf("read confirmed facts: %w", err)
	}
	if len(facts) == 0 {
		return nil, nil
	}
	r := Request{SQL: req.SQL, Targets: req.TargetObjs, Now: b.now(),
		OperatorApproved: req.OperatorApproved}
	if req.Contract != nil {
		r.ActionType = req.Contract.ActionType
	}
	refs := ResolveRefs(r)
	if b.resolver != nil && needsResolution(refs) {
		if refs, err = b.resolver.ResolveIndexes(ctx, refs); err != nil {
			return nil, fmt.Errorf("resolve index tables: %w", err)
		}
	}
	return policyBindings(Bind(facts, refs, r)), nil
}

func needsResolution(refs []ObjectRef) bool {
	for _, r := range refs {
		if r.Kind == KindRelation || (r.Kind == KindIndex && r.TableName == "") {
			return true
		}
	}
	return false
}

func policyBindings(bs []Binding) []policy.FactBinding {
	if len(bs) == 0 {
		return nil
	}
	out := make([]policy.FactBinding, 0, len(bs))
	for _, b := range bs {
		pb := policy.FactBinding{FactID: b.Fact.ID, Type: string(b.Fact.Type),
			Subject: b.Fact.Subject, Route: string(b.Route), Summary: b.Fact.Describe(),
			Object: b.Object, ConfirmedBy: b.Fact.DecidedBy}
		if b.Fact.DecidedAt != nil {
			pb.ConfirmedAt = *b.Fact.DecidedAt
		}
		out = append(out, pb)
	}
	return out
}

// CatalogResolver resolves index references from pg_class and pg_index.
type CatalogResolver struct{ pool *pgxpool.Pool }

// NewCatalogResolver resolves against the monitored database.
func NewCatalogResolver(pool *pgxpool.Pool) *CatalogResolver {
	return &CatalogResolver{pool: pool}
}

const resolveIndexesSQL = `/* pg_sage */
SELECT w.n, c.relkind::text, COALESCE(tn.nspname, ''), COALESCE(t.relname, '')
FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS w(s, r, n)
JOIN pg_catalog.pg_namespace ns ON ns.nspname = w.s
JOIN pg_catalog.pg_class c ON c.relnamespace = ns.oid AND c.relname = w.r
LEFT JOIN pg_catalog.pg_index i ON i.indexrelid = c.oid
LEFT JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace`

// ResolveIndexes implements IndexResolver: qualified relations and indexes
// found in the catalog get their kind (and an index its table); the rest
// are returned as they were.
func (c *CatalogResolver) ResolveIndexes(ctx context.Context, refs []ObjectRef) (
	[]ObjectRef, error) {
	out := append([]ObjectRef(nil), refs...)
	var schemas, names []string
	var at []int
	for i, r := range out {
		if r.Schema != "" && (r.Kind == KindRelation || r.Kind == KindIndex) {
			schemas, names, at = append(schemas, r.Schema), append(names, r.Name), append(at, i)
		}
	}
	if len(at) == 0 {
		return out, nil
	}
	rows, err := c.pool.Query(ctx, resolveIndexesSQL, schemas, names)
	if err != nil {
		return nil, fmt.Errorf("read index tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		var relkind, tSchema, tName string
		if err := rows.Scan(&n, &relkind, &tSchema, &tName); err != nil {
			return nil, fmt.Errorf("scan index table: %w", err)
		}
		applyRelkind(&out[at[n-1]], relkind, tSchema, tName)
	}
	return out, rows.Err()
}

func applyRelkind(r *ObjectRef, relkind, tSchema, tName string) {
	switch relkind {
	case "i", "I":
		r.Kind, r.TableSchema, r.TableName = KindIndex, tSchema, tName
	case "r", "p", "m", "f":
		r.Kind = KindTable
	}
}
