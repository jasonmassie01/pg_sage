package schema

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// No concurrent access tests: the check reads the catalog once per spec.

// chainTypeDrift compares the types of the columns spec's canonical
// expressions read with the pinned auditchain.SealedColumnTypes entry.
// skipped names why an optional spec's absent table was not checked; a
// required spec's absent table is an error.
func chainTypeDrift(ctx context.Context, pool *pgxpool.Pool, spec auditchain.Spec) (
	drift []string, skipped string, err error) {
	var present bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL",
		spec.Table).Scan(&present); err != nil {
		return nil, "", fmt.Errorf("look up %s: %w", spec.Table, err)
	}
	if !present {
		if spec.Optional {
			return nil, fmt.Sprintf("chain %s skipped: optional table %s is absent",
				spec.Chain, spec.Table), nil
		}
		return nil, "", fmt.Errorf("chain %s: required table %s is absent", spec.Chain,
			spec.Table)
	}
	key := auditchain.SpecKey{Chain: spec.Chain, Version: spec.Version}
	pinned, ok := auditchain.SealedColumnTypes[key]
	if !ok {
		return nil, "", fmt.Errorf("auditchain.SealedColumnTypes has no %s v%d entry",
			spec.Chain, spec.Version)
	}
	for _, col := range spec.Columns() {
		var typ string
		err := pool.QueryRow(ctx, `SELECT coalesce((SELECT format_type(atttypid, atttypmod)
			FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2
			AND attnum > 0 AND NOT attisdropped), '<missing>')`, spec.Table, col).Scan(&typ)
		if err != nil {
			return nil, "", fmt.Errorf("type of %s.%s: %w", spec.Table, col, err)
		}
		if typ != pinned[col] {
			drift = append(drift, fmt.Sprintf("%s.%s is %s, %s v%d pins %s", spec.Table, col,
				typ, spec.Chain, spec.Version, pinned[col]))
		}
	}
	return drift, "", nil
}

func bootstrappedPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	pool := freshPool(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return pool
}

// The columns every audit chain hashes have the types its payload version
// was pinned with. jsonb_build_array renders a value by its type, so a
// type change rewrites the canonical text of every historical row.
func TestAuditChainColumnTypesArePinned(t *testing.T) {
	pool := bootstrappedPool(t, "chain_types")
	ctx := context.Background()
	checked := 0
	for _, spec := range auditchain.Specs() {
		drift, skipped, err := chainTypeDrift(ctx, pool, spec)
		if err != nil {
			t.Fatal(err)
		}
		if skipped != "" {
			t.Log(skipped)
			continue
		}
		checked++
		if len(drift) > 0 {
			t.Errorf("audit chain %s column types changed:\n  %s\nEvery historical row "+
				"would verify as edited. Bump Spec.Version in internal/auditchain/spec.go "+
				"and add a SealedColumnTypes registry entry for the new version; keep the "+
				"old entry for links written under it.", spec.Chain,
				strings.Join(drift, "\n  "))
		}
	}
	if checked != len(auditchain.Specs()) {
		t.Fatalf("checked %d of %d chains on a fresh bootstrap, want all", checked,
			len(auditchain.Specs()))
	}
	v9 := auditchain.ConfigAudit
	v9.Version = 9
	if _, _, err := chainTypeDrift(ctx, pool, v9); err == nil ||
		!strings.Contains(err.Error(), "config_audit v9") {
		t.Fatalf("unpinned version: err %v, want it named", err)
	}
}

// Changing a hashed column's type is caught and named.
func TestAuditChainColumnTypeChangeIsCaught(t *testing.T) {
	pool := bootstrappedPool(t, "chain_types_drift")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.config_audit
		ALTER COLUMN database_id TYPE bigint`); err != nil {
		t.Fatalf("alter column type: %v", err)
	}
	drift, skipped, err := chainTypeDrift(ctx, pool, auditchain.ConfigAudit)
	if err != nil || skipped != "" || len(drift) != 1 ||
		!strings.Contains(drift[0], "sage.config_audit.database_id is bigint") ||
		!strings.Contains(drift[0], "pins integer") {
		t.Fatalf("drift = %v skipped %q err %v, want database_id named", drift, skipped, err)
	}
	if drift, _, err := chainTypeDrift(ctx, pool, auditchain.AuthAudit); err != nil ||
		len(drift) != 0 {
		t.Fatalf("untouched auth_audit: drift %v err %v", drift, err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE sage.auth_audit DROP COLUMN source_ip`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	drift, _, err = chainTypeDrift(ctx, pool, auditchain.AuthAudit)
	if err != nil || len(drift) != 1 || !strings.Contains(drift[0], "<missing>") {
		t.Fatalf("dropped column: drift %v err %v, want it reported missing", drift, err)
	}
}

// An optional chain whose table is absent is skipped with a reason; a
// required chain's absent table is an error.
func TestAuditChainTypesSkipOnlyAbsentOptionalTables(t *testing.T) {
	pool := bootstrappedPool(t, "chain_types_absent")
	ctx := context.Background()
	for _, sql := range []string{"DROP TABLE sage.guard_query_audit CASCADE",
		"DROP TABLE sage.guard_pgaudit_events CASCADE"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	drift, skipped, err := chainTypeDrift(ctx, pool, auditchain.GuardQueryAudit)
	if err != nil || drift != nil || !strings.Contains(skipped, "guard_query_audit is absent") {
		t.Fatalf("optional absent: drift %v skipped %q err %v", drift, skipped, err)
	}
	t.Log(skipped)
	_, skipped, err = chainTypeDrift(ctx, pool, auditchain.PGAuditEvents)
	if err == nil || skipped != "" || !strings.Contains(err.Error(), "guard_pgaudit_events") {
		t.Fatalf("required absent: skipped %q err %v, want an error", skipped, err)
	}
}
