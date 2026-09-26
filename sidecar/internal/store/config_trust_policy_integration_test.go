//go:build integration

package store

import (
	"context"
	"errors"
	"testing"
)

// G5-B14: a global trust downgrade in meta-db mode lowers each database's
// durable trust row, audits it, and drops legacy per-database overrides so
// the lowered policy survives restart.
func TestSetDatabaseTrustPolicyPersistsAuditsAndReturnsPrevious(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO sage.databases
		(id, name, host, port, database_name, username, password_enc,
		 sslmode, trust_level, execution_mode)
		VALUES (902, 'trust-cap-policy', 'localhost', 5432, 'postgres',
		 'postgres', '\x00', 'disable', 'autonomous', 'auto')
		ON CONFLICT (id) DO UPDATE SET trust_level = 'autonomous'`)
	if err != nil {
		t.Fatalf("seed database: %v", err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO sage.config (key, value, database_id)
		VALUES ('trust.level', 'autonomous', 902)`)
	if err != nil {
		t.Fatalf("seed legacy override: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, "DELETE FROM sage.config WHERE database_id = 902")
		_, _ = pool.Exec(bg, "DELETE FROM sage.config_audit WHERE database_id = 902")
		_, _ = pool.Exec(bg, "DELETE FROM sage.databases WHERE id = 902")
	})

	previous, err := cs.SetDatabaseTrustPolicy(ctx, 902, "observation")
	if err != nil {
		t.Fatalf("set trust policy: %v", err)
	}
	if previous != "autonomous" {
		t.Fatalf("previous = %q, want autonomous", previous)
	}
	var trust string
	if err := pool.QueryRow(ctx,
		"SELECT trust_level FROM sage.databases WHERE id = 902",
	).Scan(&trust); err != nil {
		t.Fatalf("read trust: %v", err)
	}
	if trust != "observation" {
		t.Fatalf("trust = %q, want observation", trust)
	}
	var audits, legacy int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config_audit
		WHERE database_id = 902 AND key = 'trust.level'
		  AND old_value = 'autonomous' AND new_value = 'observation'`,
	).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.config
		WHERE database_id = 902 AND key = 'trust.level'`).Scan(&legacy); err != nil {
		t.Fatalf("count legacy: %v", err)
	}
	if audits != 1 || legacy != 0 {
		t.Fatalf("audits=%d legacy=%d, want 1 and 0", audits, legacy)
	}
}

func TestSetDatabaseTrustPolicyRejectsUnknownDatabaseAndLevel(t *testing.T) {
	pool := setupConfigTestDB(t)
	cs := NewConfigStore(pool)
	ctx := context.Background()
	if _, err := cs.SetDatabaseTrustPolicy(ctx, 987654, "observation"); !errors.Is(
		err, ErrConfigDatabaseNotFound) {
		t.Fatalf("unknown database err = %v, want ErrConfigDatabaseNotFound", err)
	}
	if _, err := cs.SetDatabaseTrustPolicy(ctx, 902, "reckless"); err == nil {
		t.Fatal("invalid trust level accepted")
	}
}
