package lakebaselive

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type providerExtensionAttempt struct {
	Operation string `json:"operation"`
	SQLState  string `json:"sqlstate,omitempty"`
	Succeeded bool   `json:"succeeded"`
}

// This compatibility gate deliberately fails when a documented/required
// extension cannot be used. A successful refusal test is not full support.
func TestLakebaseProviderExtensionCompatibility(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_PROVIDER_COMPATIBILITY") != "1" {
		t.Skip("set PG_SAGE_LIVE_PROVIDER_COMPATIBILITY=1 for strict provider extension compatibility")
	}
	p, ctx := livePool(t)
	report := struct {
		StartedAt     time.Time                  `json:"started_at"`
		FinishedAt    time.Time                  `json:"finished_at"`
		Attempts      []providerExtensionAttempt `json:"attempts"`
		HintLoaded    bool                       `json:"hint_hook_loaded"`
		HypoAvailable bool                       `json:"hypopg_advertised"`
		Fallback      string                     `json:"tested_fallback,omitempty"`
	}{StartedAt: time.Now().UTC()}
	t.Cleanup(func() {
		report.FinishedAt = time.Now().UTC()
		writeProviderCompatibilityReport(t, report)
	})
	t.Run("HintActivation", func(t *testing.T) {
		conn, err := p.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		for _, attempt := range []struct{ name, statement string }{
			{"create_pg_hint_plan", "CREATE EXTENSION IF NOT EXISTS pg_hint_plan"},
			{"load_pg_hint_plan", "LOAD 'pg_hint_plan'"},
			{"session_preload_pg_hint_plan", "SET LOCAL session_preload_libraries='pg_hint_plan'"},
			{"database_preload_pg_hint_plan", "ALTER DATABASE pg_sage_test SET session_preload_libraries='pg_hint_plan'"},
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT extension_probe"); err != nil {
				t.Fatal(err)
			}
			_, attemptErr := tx.Exec(ctx, attempt.statement)
			result := providerExtensionAttempt{Operation: attempt.name, Succeeded: attemptErr == nil}
			if attemptErr != nil {
				var pgErr *pgconn.PgError
				if !errors.As(attemptErr, &pgErr) {
					t.Fatalf("%s failed without SQLSTATE: %v", attempt.name, attemptErr)
				}
				result.SQLState = pgErr.Code
			}
			report.Attempts = append(report.Attempts, result)
			t.Logf("operation=%s succeeded=%v SQLSTATE=%s", result.Operation, result.Succeeded, result.SQLState)
			// Every configuration attempt is rolled back; this never leaves a
			// database setting for other test sessions or the App principal.
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT extension_probe"); err != nil {
				t.Fatal(err)
			}
		}
		if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_settings WHERE name='pg_hint_plan.enable_hint')").Scan(&report.HintLoaded); err != nil {
			t.Fatal(err)
		}
		if !report.HintLoaded {
			t.Error("observed provider incompatibility: documented pg_hint_plan installs but no supported activation path enabled its hook")
		}
	})
	t.Run("HypoPGInstallation", func(t *testing.T) {
		if err := p.QueryRow(ctx, "SELECT EXISTS(SELECT FROM pg_available_extensions WHERE name='hypopg')").Scan(&report.HypoAvailable); err != nil {
			t.Fatal(err)
		}
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS hypopg")
		result := providerExtensionAttempt{Operation: "create_hypopg", Succeeded: err == nil}
		if err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatal(err)
			}
			result.SQLState = pgErr.Code
		}
		report.Attempts = append(report.Attempts, result)
		t.Logf("HypoPG advertised=%v CREATE succeeded=%v SQLSTATE=%s", report.HypoAvailable, result.Succeeded, result.SQLState)
		if !result.Succeeded {
			t.Error("observed provider incompatibility: HypoPG what-if index validation cannot run on this Lakebase image")
		}
	})
	t.Run("PhysicalIndexRollbackFallback", func(t *testing.T) {
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		for _, statement := range []string{
			"CREATE TEMP TABLE provider_index_fallback(id integer, category integer) ON COMMIT DROP",
			"INSERT INTO provider_index_fallback SELECT i, i%100 FROM generate_series(1,20000)i",
			"ANALYZE provider_index_fallback",
			"SAVEPOINT candidate_index",
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		cost := func() float64 {
			t.Helper()
			var raw []byte
			if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) SELECT id FROM provider_index_fallback WHERE category=42").Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var docs []struct {
				Plan struct {
					Cost float64 `json:"Total Cost"`
				} `json:"Plan"`
			}
			if err := json.Unmarshal(raw, &docs); err != nil || len(docs) != 1 {
				t.Fatal("invalid own fallback EXPLAIN")
			}
			return docs[0].Plan.Cost
		}
		before := cost()
		if _, err := tx.Exec(ctx, "CREATE INDEX provider_index_fallback_category ON provider_index_fallback(category)"); err != nil {
			t.Fatal(err)
		}
		after := cost()
		if after >= before {
			t.Fatalf("own physical candidate did not lower estimated cost: before=%g after=%g", before, after)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT candidate_index"); err != nil {
			t.Fatal(err)
		}
		if restored := cost(); restored != before {
			t.Fatalf("candidate rollback did not restore original plan cost: %g != %g", restored, before)
		}
		report.Fallback = "owned physical index reduced estimated cost and rollback restored baseline; not an in-place HypoPG replacement or measured workload benefit"
		t.Logf("owned index fallback before_cost=%g indexed_cost=%g rollback_restored=true", before, after)
	})
}

func writeProviderCompatibilityReport(t *testing.T, report any, reportEnv ...string) {
	t.Helper()
	name := "SAGE_PROVIDER_COMPATIBILITY_REPORT"
	if len(reportEnv) > 0 {
		name = reportEnv[0]
	}
	path := os.Getenv(name)
	if path == "" {
		return
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Error(err)
		return
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
		err = os.WriteFile(path, append(raw, '\n'), 0600)
	}
	if err != nil {
		t.Error(err)
	}
}
