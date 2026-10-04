package schema

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Roadmap 2.4 owner addition A (2026-10-04): the trust ledger's history
// records a family earning or losing model-root authority
// (root_authority_granted, root_authority_revoked). Several migrations
// rebuild sre_autonomy_events_type_v2 in bootstrap order; whatever the
// order, the final check must admit every event type internal/earned
// writes. The types are read from the source, so a new ledger event type
// without a migration fails here.

// autonomyEventTypes are the EventType constants of internal/earned.
func autonomyEventTypes(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join("..", "earned")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		collectTypedConsts(f, "EventType", seen)
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func collectTypedConsts(f *ast.File, typ string, seen map[string]bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if id, ok := spec.Type.(*ast.Ident); !ok || id.Name != typ {
			return true
		}
		for _, v := range spec.Values {
			if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					seen[s] = true
				}
			}
		}
		return true
	})
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func autonomyTypeCheck(t *testing.T, q rowQuerier) string {
	t.Helper()
	var def string
	if err := q.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conname = 'sre_autonomy_events_type_v2'
		  AND conrelid = 'sage.sre_autonomy_events'::regclass`).Scan(&def); err != nil {
		t.Fatalf("read sre_autonomy_events_type_v2: %v", err)
	}
	return def
}

func TestSREMigrationRootAuthority_AdmitsEveryLedgerEventType(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	types := autonomyEventTypes(t)
	for _, want := range []string{"root_authority_granted", "root_authority_revoked",
		"grandfathered", "promotion_proposed"} {
		if !contains(types, want) {
			t.Fatalf("internal/earned writes no %q event (types %v)", want, types)
		}
	}
	def := autonomyTypeCheck(t, pool)
	for _, typ := range types {
		if !strings.Contains(def, "'"+typ+"'") {
			t.Errorf("sre_autonomy_events_type_v2 does not admit %q: %s", typ, def)
		}
	}
}

// A database bootstrapped before the addition (the check as the trust
// ledger migration left it) gains the new types; a re-run changes
// nothing.
func TestSREMigrationRootAuthority_UpgradesTheOlderCheckIdempotently(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	// In a transaction rolled back at the end: other packages share the
	// database and must never see the older check.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `ALTER TABLE sage.sre_autonomy_events
		DROP CONSTRAINT IF EXISTS sre_autonomy_events_type_v2;
		ALTER TABLE sage.sre_autonomy_events
		ADD CONSTRAINT sre_autonomy_events_type_v2 CHECK (event_type IN (
			'promotion_proposed', 'promotion_approved', 'promotion_rejected',
			'promotion_expired', 'downgraded', 'auto_downgraded', 'capped',
			'cap_cleared', 'auto_executed', 'carried_over', 'deadline_override',
			'database_scoped', 'grandfathered')) NOT VALID`); err != nil {
		t.Fatalf("restore the older check: %v", err)
	}
	if _, err := tx.Exec(ctx, ddlSREAutonomyRootAuthority); err != nil {
		t.Fatalf("migration: %v", err)
	}
	first := autonomyTypeCheck(t, tx)
	for _, typ := range []string{"root_authority_granted", "root_authority_revoked",
		"grandfathered", "database_scoped", "carried_over", "promotion_proposed"} {
		if !strings.Contains(first, "'"+typ+"'") {
			t.Fatalf("after the migration the check lacks %q: %s", typ, first)
		}
	}
	if _, err := tx.Exec(ctx, ddlSREAutonomyRootAuthority); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if again := autonomyTypeCheck(t, tx); again != first {
		t.Fatalf("a re-run changed the check: %s / %s", first, again)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
