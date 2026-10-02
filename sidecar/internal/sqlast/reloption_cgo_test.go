//go:build cgo

package sqlast

import (
	"errors"
	"strings"
	"testing"
)

// The parse-tree layer sees reloption names and values exactly as
// PostgreSQL will (quoted, Unicode-escaped, namespaced), so the
// executor's reloption rule is enforced here too (G-P0-1).
func TestCheckReloptionRule(t *testing.T) {
	var seen []string
	rules := testRules
	rules.Reloption = func(key, value string, reset bool) bool {
		seen = append(seen, key+"="+value)
		return !(strings.HasSuffix(key, "autovacuum_enabled") && value == "false") &&
			key != "parallel_workers"
	}
	refused := []string{
		`ALTER TABLE public.t SET (autovacuum_enabled = false)`,
		`ALTER TABLE public.t SET (U&"autovacuum\005fenabled" = false)`,
		`ALTER TABLE public.t SET (toast.autovacuum_enabled = false)`,
		`ALTER TABLE public.t SET (fillfactor = 90, parallel_workers = 2)`,
		`ALTER TABLE public.t RESET (parallel_workers)`,
	}
	for _, sql := range refused {
		if err := Check(sql, rules); !errors.Is(err, ErrRejected) {
			t.Errorf("Check(%q) = %v, want rejection", sql, err)
		}
	}
	seen = nil
	if err := Check(`ALTER TABLE public.t SET (toast.autovacuum_vacuum_threshold = 50, `+
		`fillfactor = 90)`, rules); err != nil {
		t.Fatalf("allowed reloptions rejected: %v", err)
	}
	if strings.Join(seen, ",") != "toast.autovacuum_vacuum_threshold=50,fillfactor=90" {
		t.Errorf("rule saw %v, want namespaced keys with their values", seen)
	}
	seen = nil
	if err := Check(`ALTER TABLE public.t SET (autovacuum_enabled)`, rules); err != nil {
		t.Fatalf("bare boolean option rejected: %v", err)
	}
	if len(seen) != 1 || seen[0] != "autovacuum_enabled=true" {
		t.Errorf("bare option value = %v, want true", seen)
	}
}

// Without a reloption rule the layer keeps its old behavior.
func TestCheckReloptionRuleOptional(t *testing.T) {
	if err := Check(`ALTER TABLE public.t SET (parallel_workers = 2)`, testRules); err != nil {
		t.Fatalf("nil reloption rule rejected: %v", err)
	}
}
