package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/migration"
)

// migration-10: the enricher writes model output into the open finding's
// llm_ keys after the deterministic upsert; a later re-detection keeps
// them, and a new enrichment replaces all of them.

func readMigrationDetail(
	t *testing.T, ctx context.Context, ident string,
) (map[string]any, int) {
	t.Helper()
	pool, _ := requireMigrationSafetyDB(t)
	var raw []byte
	var occurrences int
	err := pool.QueryRow(ctx,
		`SELECT detail, occurrence_count FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2 AND status = 'open'`,
		migration.SafetyFindingCategory, ident).Scan(&raw, &occurrences)
	if err != nil {
		t.Fatalf("read detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	return detail, occurrences
}

func TestMigrationLLMDetailSurvivesRedetection(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	ident := fmt.Sprintf("public.orders:llm:%d", time.Now().UnixNano())
	cleanupMigrationIdent(t, ident)
	s := NewMigrationSafetyFindingStore(pool)
	if _, err := s.UpsertMigrationSafetyFinding(ctx, migrationUpsertInput(ident)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.MergeMigrationFindingLLMDetail(ctx, ident, map[string]any{
		"llm_script_invalid": "statement 1 is not allowed", "llm_unvalidated_text": "GRANT x",
	}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	detail, occ := readMigrationDetail(t, ctx, ident)
	if detail["llm_unvalidated_text"] != "GRANT x" || detail["risk_score"] != 0.6 || occ != 1 {
		t.Fatalf("after merge detail = %v occurrences = %d; want both kinds of key, 1 occurrence",
			detail, occ)
	}

	again := migrationUpsertInput(ident)
	again.Detail = map[string]any{"risk_score": 0.8}
	if _, err := s.UpsertMigrationSafetyFinding(ctx, again); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	detail, occ = readMigrationDetail(t, ctx, ident)
	if detail["llm_script_invalid"] != "statement 1 is not allowed" ||
		detail["risk_score"] != 0.8 || occ != 2 {
		t.Fatalf("after re-detection detail = %v occurrences = %d; want llm_ keys kept",
			detail, occ)
	}

	if err := s.MergeMigrationFindingLLMDetail(ctx, ident,
		map[string]any{"llm_script": "ANALYZE orders"}); err != nil {
		t.Fatalf("second merge: %v", err)
	}
	detail, _ = readMigrationDetail(t, ctx, ident)
	if detail["llm_script"] != "ANALYZE orders" || detail["llm_script_invalid"] != nil ||
		detail["llm_unvalidated_text"] != nil || detail["risk_score"] != 0.8 {
		t.Fatalf("after second merge detail = %v; want only the new llm_ keys", detail)
	}
}

// An upsert that carries llm_ keys (the fallback finding) owns them all.
func TestMigrationUpsertWithLLMKeysReplacesThem(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	ident := fmt.Sprintf("unknown:llm_ddl_fallback:%d", time.Now().UnixNano())
	cleanupMigrationIdent(t, ident)
	s := NewMigrationSafetyFindingStore(pool)
	first := migrationUpsertInput(ident)
	first.Detail = map[string]any{"llm_safe_alternative": "ANALYZE t"}
	second := migrationUpsertInput(ident)
	second.Detail = map[string]any{"llm_script_invalid": "why", "llm_unvalidated_text": "x"}
	for _, in := range []migration.MigrationSafetyFinding{first, second} {
		if _, err := s.UpsertMigrationSafetyFinding(ctx, in); err != nil {
			t.Fatalf("upsert: %v", err)
		}
	}
	detail, _ := readMigrationDetail(t, ctx, ident)
	if detail["llm_safe_alternative"] != nil || detail["llm_script_invalid"] != "why" {
		t.Fatalf("detail = %v, want the second upsert's llm_ keys only", detail)
	}
}

// A merge never reopens or touches a finding that is no longer open.
func TestMigrationLLMDetailSkipsClosedFindings(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	ident := fmt.Sprintf("public.orders:closed:%d", time.Now().UnixNano())
	cleanupMigrationIdent(t, ident)
	s := NewMigrationSafetyFindingStore(pool)
	if _, err := s.UpsertMigrationSafetyFinding(ctx, migrationUpsertInput(ident)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status = 'resolved'
		WHERE object_identifier = $1`, ident); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := s.MergeMigrationFindingLLMDetail(ctx, ident,
		map[string]any{"llm_script": "ANALYZE orders"}); err != nil {
		t.Fatalf("merge on a resolved finding: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE object_identifier = $1 AND detail ? 'llm_script'`, ident).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d resolved findings received model output, want 0", n)
	}
}

func TestMigrationLLMDetailNilStore(t *testing.T) {
	var s *MigrationSafetyFindingStore
	err := s.MergeMigrationFindingLLMDetail(context.Background(), "x", map[string]any{})
	if err == nil {
		t.Fatal("nil store merged without error")
	}
}
