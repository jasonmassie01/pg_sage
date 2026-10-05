package schema

import (
	"context"
	"strings"
	"testing"
)

// Self-configuration (roadmap phase 3), monitored database:
// sage.config_derived_setting holds one row per derived key (its status,
// in-force, active, shadow, pending and pinned values, the latest evidence
// and bounds); sage.config_derivation is the append-only derivation
// ledger. Additive and idempotent.

func selfConfigCleanup(t *testing.T) {
	t.Helper()
	pool, _ := requireDB(t)
	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM sage.config_derivation WHERE key LIKE 'mig.%'")
		_, _ = pool.Exec(ctx, "DELETE FROM sage.config_derived_setting WHERE key LIKE 'mig.%'")
	}
	clean()
	t.Cleanup(clean)
}

func TestSelfConfigMigrationIsIdempotent(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 3; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	for _, table := range []string{"config_derived_setting", "config_derivation"} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'sage' AND table_name = $1`, table).Scan(&n); err != nil ||
			n != 1 {
			t.Fatalf("table %s: %d (%v)", table, n, err)
		}
	}
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'sage' AND c.relname = 'config_derivation_key_recent'`).
		Scan(&valid); err != nil || !valid {
		t.Fatalf("ledger index: valid=%v (%v)", valid, err)
	}
}

const insertDerivedSetting = `INSERT INTO sage.config_derived_setting
	(key, status, value, shadow_value, shadow_since, pinned_value, pinned_at, rule,
	 rule_version)
	VALUES ($1, $2, 60, $3, $4, $5, $6, 'r', $7)`

func TestDerivedSettingChecks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	selfConfigCleanup(t)
	if _, err := pool.Exec(ctx, insertDerivedSetting, "mig.ok", "shadow", 90, "2026-10-04",
		nil, nil, 1); err != nil {
		t.Fatalf("valid row refused: %v", err)
	}
	for name, args := range map[string][]any{
		"status":                {"mig.a", "maybe", nil, nil, nil, nil, 1},
		"shadow without since":  {"mig.b", "shadow", 90, nil, nil, nil, 1},
		"since without shadow":  {"mig.c", "default", nil, "2026-10-04", nil, nil, 1},
		"shadow status, no val": {"mig.d", "shadow", nil, nil, nil, nil, 1},
		"pinned without value":  {"mig.e", "pinned", nil, nil, nil, nil, 1},
		"pin value without at":  {"mig.f", "default", nil, nil, 30, nil, 1},
		"rule version zero":     {"mig.g", "default", nil, nil, nil, nil, 0},
		"bad key":               {"Mig Bad", "default", nil, nil, nil, nil, 1},
	} {
		if _, err := pool.Exec(ctx, insertDerivedSetting, args...); err == nil ||
			!strings.Contains(err.Error(), "check constraint") {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestDerivationLedgerChecks(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	selfConfigCleanup(t)
	insert := `INSERT INTO sage.config_derivation (key, event, value, rule, rule_version)
		VALUES ($1, $2, 1, 'r', 1)`
	for _, event := range []string{"shadow", "promoted", "applied", "held", "cleared",
		"operator_set", "resumed", "pinned", "unpinned"} {
		if _, err := pool.Exec(ctx, insert, "mig.k", event); err != nil {
			t.Errorf("event %s refused: %v", event, err)
		}
	}
	if _, err := pool.Exec(ctx, insert, "mig.k", "deleted"); err == nil ||
		!strings.Contains(err.Error(), "check constraint") {
		t.Fatalf("unknown event accepted: %v", err)
	}
}
