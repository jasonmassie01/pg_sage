package schema

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Sage SRE M6 integration: several branches extend sre_events'
// event-type check (M3 model events, M5 action events) and the bootstrap
// rebuilds it in order. Whatever the order, the final check must admit
// every event type the Go code writes. The types are read from the
// source, so a new event type without a migration fails here.

// sreEventTypes returns the event types internal/sre and
// internal/sre/action write: string constants named Event* in
// internal/sre and the typ fields of action events.
func sreEventTypes(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	fset := token.NewFileSet()
	for _, dir := range []string{"../sre", "../sre/action"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			collectEventTypes(f, seen)
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func collectEventTypes(f *ast.File, seen map[string]bool) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.ValueSpec:
			for i, name := range x.Names {
				if strings.HasPrefix(name.Name, "Event") && i < len(x.Values) {
					addLiteral(x.Values[i], seen)
				}
			}
		case *ast.KeyValueExpr:
			if k, ok := x.Key.(*ast.Ident); ok && k.Name == "typ" {
				addLiteral(x.Value, seen)
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if s, ok := lhs.(*ast.SelectorExpr); ok && s.Sel.Name == "typ" &&
					i < len(x.Rhs) {
					addLiteral(x.Rhs[i], seen)
				}
			}
		}
		return true
	})
}

func addLiteral(e ast.Expr, seen map[string]bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return
	}
	if v, err := strconv.Unquote(lit.Value); err == nil {
		seen[v] = true
	}
}

var quotedValue = regexp.MustCompile(`'([a-z0-9_]+)'`)

// eventTypeCheck returns the allowed values of the single event-type
// check on sage.sre_events, failing when there is not exactly one named
// sre_events_event_type_m3.
func eventTypeCheck(t *testing.T) map[string]bool {
	t.Helper()
	pool, ctx := requireDB(t)
	rows, err := pool.Query(ctx, `SELECT conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE conrelid = 'sage.sre_events'::regclass
		  AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%event_type%'`)
	if err != nil {
		t.Fatalf("read event-type checks: %v", err)
	}
	defer rows.Close()
	var names []string
	allowed := map[string]bool{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
		for _, m := range quotedValue.FindAllStringSubmatch(def, -1) {
			allowed[m[1]] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(names) != 1 || names[0] != "sre_events_event_type_m3" {
		t.Fatalf("event-type checks = %v, want only sre_events_event_type_m3", names)
	}
	return allowed
}

func assertAdmitsEveryType(t *testing.T, types []string) {
	t.Helper()
	allowed := eventTypeCheck(t)
	for _, typ := range types {
		if !allowed[typ] {
			t.Errorf("sre_events check does not admit event type %q (allowed: %v)",
				typ, allowed)
		}
	}
}

func TestSREEventTypes_SourceScanFindsTheKnownTypes(t *testing.T) {
	types := sreEventTypes(t)
	have := map[string]bool{}
	for _, v := range types {
		have[v] = true
	}
	// A floor, so a broken scanner cannot pass the union test vacuously.
	for _, want := range []string{"created", "concluded", "model_rejected",
		"action_proposed", "action_failed", "recovery_verdict"} {
		if !have[want] {
			t.Errorf("source scan missed %q; found %v", want, types)
		}
	}
	if len(types) < 20 {
		t.Fatalf("source scan found %d event types, want at least 20: %v", len(types), types)
	}
}

func TestSREEventTypes_BootstrapAdmitsEveryType(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	assertAdmitsEveryType(t, sreEventTypes(t))
}

// An install that stopped at the M2 inline check (no M3 or M5 types)
// upgrades to a check that admits every type, and a second bootstrap
// leaves it unchanged.
func TestSREEventTypes_UpgradeFromM2CheckAdmitsEveryType(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, stmt := range []string{
		`ALTER TABLE sage.sre_events DROP CONSTRAINT IF EXISTS sre_events_event_type_m3`,
		`ALTER TABLE sage.sre_events DROP CONSTRAINT IF EXISTS sre_events_event_type_check`,
		`DELETE FROM sage.sre_events WHERE event_type NOT IN ('created', 'claimed',
			'step', 'transition', 'concluded', 'pinned', 'unpinned', 'evidence_purged')`,
		`ALTER TABLE sage.sre_events ADD CONSTRAINT sre_events_event_type_check
			CHECK (event_type IN ('created', 'claimed', 'step', 'transition',
			'concluded', 'pinned', 'unpinned', 'evidence_purged'))`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("simulate the M2 check: %v", err)
		}
	}
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
		assertAdmitsEveryType(t, sreEventTypes(t))
	}
}
