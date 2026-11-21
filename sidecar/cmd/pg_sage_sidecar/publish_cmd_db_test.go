package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence"
	"github.com/pg-sage/sidecar/internal/publish"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/verify/backtest"
)

// pub-01 on real PostgreSQL (run on PG14 and PG17): a ledger seeded with
// secret-like literals, the database's name and a most-common value gives
// a bundle that holds none of them in any file, and counts that equal the
// seeded rows.

// The literals a public bundle must never carry.
const (
	seedSecret = "sk_live_pgsage_fake_7Qx9Lm2"
	seedAWSKey = "AKIAPGSAGEFAKE7Q2X9L"
	seedMCV    = "mcv_lisbon_customer_4242"
	seedType   = "custom_sk_live_type"
)

func publishDB(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, "pub01")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return pool, dsn, cfg.ConnConfig.Database
}

// seedChange inserts one executed change carrying the leak literals in its
// SQL, justification and rollback, and its verdict when verdict is set.
func seedChange(t *testing.T, pool *pgxpool.Pool, dbName, actionType, outcome,
	verdict string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	sql := "CREATE INDEX CONCURRENTLY " + seedMCV + "_idx ON " + dbName +
		".public.customers (city) WHERE city = '" + seedMCV + "'"
	why := "token=" + seedSecret + " aws=" + seedAWSKey + " database " + dbName
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log (executed_at, action_type,
		sql_executed, rollback_sql, outcome, justification)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, at, actionType, sql,
		"DROP INDEX CONCURRENTLY "+seedMCV+"_idx", outcome, why).Scan(&id)
	if err != nil {
		t.Fatalf("seed action: %v", err)
	}
	if verdict == "" {
		return
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_outcome (action_log_id,
		action_class, predicted, prediction_method, verdict, observed, reason)
		VALUES ($1, 'index_create', '{"class":"index_create","method":"hypopg",
		"metric":"blocks_per_call","expected_change_pct":-40}', 'hypopg', $2,
		'{"metric":"blocks_per_call","change_pct":-35}', $3)`, id, verdict,
		"most common value "+seedMCV+" in "+dbName); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
}

func seedPublishLedger(t *testing.T, pool *pgxpool.Pool, dbName string) {
	t.Helper()
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		seedChange(t, pool, dbName, "create_index_concurrently", "success", "improved",
			now.Add(-time.Duration(i+1)*time.Hour))
	}
	seedChange(t, pool, dbName, "create_index_concurrently", "success", "neutral",
		now.Add(-6*time.Hour))
	seedChange(t, pool, dbName, "create_index_concurrently", "rolled_back", "regressed",
		now.Add(-7*time.Hour))
	seedChange(t, pool, dbName, "analyze_table", "success", "improved", now.Add(-8*time.Hour))
	seedChange(t, pool, dbName, "analyze_table", "success", "", now.Add(-9*time.Hour))
	seedChange(t, pool, dbName, seedType, "success", "", now.Add(-10*time.Hour))
	seedChange(t, pool, dbName, "create_index_concurrently", "failed", "",
		now.Add(-11*time.Hour))
	seedChange(t, pool, dbName, "create_index_concurrently", "success", "improved",
		now.Add(-120*24*time.Hour))
	if _, err := backtest.Save(context.Background(), pool, 0, backtest.Result{
		Database: dbName, RanAt: now.Add(-time.Hour), From: now.Add(-90 * 24 * time.Hour),
		To: now, Seed: 7, Verifier: "v2",
		V2: backtest.Counts{Placebos: 250, Excluded: 10, FalsePositives: 6}}); err != nil {
		t.Fatalf("seed back-test: %v", err)
	}
}

func TestPublishOutcomes_DBBundleCarriesNoSeededValue(t *testing.T) {
	pool, dsn, dbName := publishDB(t)
	seedPublishLedger(t, pool, dbName)
	pemText, pub := publishKeyPEM(t)
	deps := defaultPublishDeps(func(string) string { return "" })
	deps.lookupSecret = func(name string) (string, error) {
		if name == defaultPublishKeyEnv {
			return pemText, nil
		}
		return "", nil
	}
	out := filepath.Join(t.TempDir(), "bundle")
	code, _, stderr := runPublish(t, deps, "outcomes", "--since", "90d", "--out", out,
		"--dsn", dsn)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	files := bundleBytes(t, out)
	if len(files) < 6 {
		t.Fatalf("bundle holds %d files, want at least 6", len(files))
	}
	for name, body := range files {
		for _, leak := range []string{seedSecret, seedAWSKey, seedMCV, seedType, dbName,
			"sk_live", "AKIA", "lisbon", "customers"} {
			if bytes.Contains(body, []byte(leak)) {
				t.Errorf("%s carries %q", name, leak)
			}
		}
	}
	pack, _ := os.ReadFile(filepath.Join(out, publish.PackName))
	if _, err := evidence.Verify(bytes.NewReader(pack), pub); err != nil {
		t.Fatalf("the bundle does not verify: %v", err)
	}
	assertSeededCounts(t, string(files[publish.JSONName]))
}

func assertSeededCounts(t *testing.T, raw string) {
	t.Helper()
	for _, want := range []string{
		`"changes": 9`, `"improved": 5`, `"no_change": 1`, `"regressed": 1`,
		`"pending": 2`, `"reverted": 1`, `"key": "create_index_concurrently"`,
		`"key": "other"`, `"usable_placebos": 240`, `"false_positives": 6`,
		`"fp_rate": 0.025`, `"databases": 1`} {
		if !strings.Contains(raw, want) {
			t.Errorf("outcomes.json lacks %s:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, `"key": "analyze_table"`) {
		t.Error("analyze_table has 2 changes, under the floor, but is named")
	}
}

func TestPublishOutcomes_DBReadFailureWritesNothing(t *testing.T) {
	pool, dsn, _ := publishDB(t)
	drop := `DROP TABLE sage.action_outcome CASCADE`
	if _, err := pool.Exec(context.Background(), drop); err != nil {
		t.Fatal(err)
	}
	pemText, _ := publishKeyPEM(t)
	deps := defaultPublishDeps(func(string) string { return "" })
	deps.lookupSecret = func(string) (string, error) { return pemText, nil }
	dir := t.TempDir()
	code, _, stderr := runPublish(t, deps, "outcomes", "--out", filepath.Join(dir, "b"),
		"--dsn", dsn)
	if code != 1 || !strings.Contains(stderr, "action_outcome") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the missing table", code, stderr)
	}
	assertDirHolds(t, dir)
}

// bundleBytes is every bundle file plus each member of the pack, so the
// scan also covers the compressed copy.
func bundleBytes(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = body
	}
	gz, err := gzip.NewReader(bytes.NewReader(files[publish.PackName]))
	if err != nil {
		t.Fatalf("open the pack: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[publish.PackName+"/"+h.Name] = body
	}
}
