package schema

import (
	"strings"
	"testing"
)

// market-12: sage.managed_change_proposals accepts CloudNativePG Cluster
// spec proposals (provider cnpg); every earlier provider stays accepted and
// unknown names stay refused.
//
// No concurrent access tests: Bootstrap serializes migrations under its
// advisory lock (covered by the bootstrap lock tests).

func TestManagedChangeProvidersCNPG_BootstrapTwiceAcceptsCNPG(t *testing.T) {
	pool, ctx := requireDB(t)
	for run := 0; run < 2; run++ {
		bootstrapWithRetry(t, ctx, pool)
	}
	def, validated := managedChangeProviderCheck(t, ctx, pool)
	if !validated {
		t.Fatalf("provider check %q is not validated", def)
	}
	for _, p := range []string{"rds", "aurora", "cloud-sql", "alloydb", "azure", "cnpg"} {
		if !strings.Contains(def, "'"+p+"'") {
			t.Fatalf("provider check %q does not allow %q", def, p)
		}
	}
	clean := func() {
		_, _ = pool.Exec(ctx,
			"DELETE FROM sage.managed_change_proposals WHERE fingerprint LIKE 'adv03-%'")
	}
	clean()
	t.Cleanup(clean)
	if err := insertManagedChange(ctx, pool, "cnpg"); err != nil {
		t.Fatalf("provider cnpg refused: %v", err)
	}
	for _, p := range []string{"cloudnative-pg", "CNPG", "neon"} {
		err := insertManagedChange(ctx, pool, p)
		if err == nil || !strings.Contains(err.Error(), "managed_change_proposals_provider_check") {
			t.Fatalf("provider %q accepted (err %v)", p, err)
		}
	}
}

func TestManagedChangeProvidersCNPG_IsBoundedAndIdempotent(t *testing.T) {
	sql := ddlManagedChangeProvidersCNPG
	for _, want := range []string{"SET LOCAL lock_timeout", "NOT VALID",
		"VALIDATE CONSTRAINT", "'cnpg'", "IF NOT EXISTS"} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration lacks %q", want)
		}
	}
}
