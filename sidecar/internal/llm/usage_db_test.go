//go:build integration

package llm_test

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// llm-04: the attributed usage ledger in sage.llm_usage on a real
// PostgreSQL. These tests rename sage.llm_usage, so run this package's
// integration tests on their own (not in parallel with other packages).

func usageDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// usageDBFake answers every request with 1200 prompt and 300 completion
// tokens. It never reaches a provider.
func usageDBFake(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,` +
			`"completion_tokens":300,"total_tokens":1500}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func ledgerClient(t *testing.T, endpoint, model, database string,
	acc *llm.UsageAccumulator) *llm.Client {
	t.Helper()
	c := llm.New(&config.LLMConfig{Enabled: true, Endpoint: endpoint, APIKey: "k",
		Model: model, TimeoutSeconds: 5, TokenBudgetDaily: 10_000_000},
		func(string, string, ...any) {})
	c.SetObserver(func(o llm.Outcome) { acc.Record(database, "general", o) })
	return c
}

func nameKey(database string) string { return "name:" + database }

type purposeTotals struct {
	prompt, completion, calls, failures int64
	cost                                *float64
	source                              string
}

func readTotals(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	database string) map[string]purposeTotals {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT purpose || '/' || model, prompt_tokens,
		completion_tokens, calls, failures, est_cost_usd::float8, source
		FROM sage.llm_usage WHERE database_key = $1 AND purpose <> '_total'`,
		nameKey(database))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	out := map[string]purposeTotals{}
	for rows.Next() {
		var key string
		var v purposeTotals
		if err := rows.Scan(&key, &v.prompt, &v.completion, &v.calls, &v.failures,
			&v.cost, &v.source); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[key] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func cleanupDatabase(t *testing.T, pool *pgxpool.Pool, database string) {
	t.Helper()
	clear := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.llm_usage WHERE database_key = $1`, nameKey(database))
	}
	clear()
	t.Cleanup(clear)
}

func chatN(t *testing.T, c *llm.Client, purpose string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ctx := llm.WithPurpose(context.Background(), purpose)
		if _, _, err := c.Chat(ctx, "system", fmt.Sprintf("%s %d %d", purpose, i,
			time.Now().UnixNano()), 400); err != nil {
			t.Fatalf("chat %s %d: %v", purpose, i, err)
		}
	}
}

// Integration: a restart mid-day keeps the totals by purpose (each
// process adds its deltas), and the cost is pricing x tokens to the cent.
func TestUsageDB_RestartMidDayTotalsSurviveAndCostMatches(t *testing.T) {
	pool, ctx := usageDB(t)
	const database = "llm04_restart"
	cleanupDatabase(t, pool, database)
	in, out := 2.50, 10.00
	pricing := llm.PricingFrom(map[string]config.LLMPrice{
		"priced-model": {InputPerM: &in, OutputPerM: &out}})
	sink := store.LLMAttributionSink(store.NewLLMUsageStore(pool), nameKey)
	endpoint := usageDBFake(t)
	for process, calls := range []int{3, 2} { // the second process is the restart
		acc := llm.NewUsageAccumulator(llm.DefaultUsageMaxKeys, pricing,
			func(string, string, ...any) {})
		chatN(t, ledgerClient(t, endpoint, "priced-model", database, acc), "briefing", calls)
		chatN(t, ledgerClient(t, endpoint, "free-model", database, acc), "explain", 1)
		if err := acc.Flush(ctx, sink); err != nil {
			t.Fatalf("process %d flush: %v", process, err)
		}
	}
	got := readTotals(t, ctx, pool, database)
	b := got["briefing/priced-model"]
	if b.prompt != 6000 || b.completion != 1500 || b.calls != 5 || b.failures != 0 ||
		b.source != "provider" {
		t.Fatalf("briefing totals = %+v, want 6000/1500 over 5 calls from the provider", b)
	}
	wantCost := 5 * (1200*in + 300*out) / 1e6
	if b.cost == nil || math.Round(*b.cost*100) != math.Round(wantCost*100) {
		t.Fatalf("briefing cost = %v, want %.4f to the cent", b.cost, wantCost)
	}
	e := got["explain/free-model"]
	if e.calls != 2 || e.prompt != 2400 || e.cost != nil {
		t.Fatalf("explain totals = %+v, want 2 calls and NULL cost (no price)", e)
	}
	var budgetRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.llm_usage
		WHERE database_key = $1 AND purpose = '_total'`, nameKey(database)).
		Scan(&budgetRows); err != nil || budgetRows != 0 {
		t.Fatalf("attribution wrote %d budget rows (err %v), want 0", budgetRows, err)
	}
}

// Error propagation: with the table gone, Chat still succeeds, the flush
// fails with one log line, and the next successful flush carries the
// retained deltas.
func TestUsageDB_MissingTableKeepsChatAndRetainsDeltas(t *testing.T) {
	pool, ctx := usageDB(t)
	const database = "llm04_missing"
	cleanupDatabase(t, pool, database)
	var mu sync.Mutex
	var lines []string
	acc := llm.NewUsageAccumulator(llm.DefaultUsageMaxKeys, llm.PricingFrom(nil),
		func(_ string, format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		})
	c := ledgerClient(t, usageDBFake(t), "m", database, acc)
	sink := store.LLMAttributionSink(store.NewLLMUsageStore(pool), nameKey)
	hide := `ALTER TABLE sage.llm_usage RENAME TO llm_usage_llm04`
	if _, err := pool.Exec(ctx, hide); err != nil {
		t.Fatalf("hide table: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			_, _ = pool.Exec(context.Background(),
				`ALTER TABLE sage.llm_usage_llm04 RENAME TO llm_usage`)
		}
	}
	t.Cleanup(restore)
	for round := 0; round < 2; round++ {
		chatN(t, c, "facts", 2) // chatN fails the test if Chat fails
		if err := acc.Flush(ctx, sink); err == nil {
			t.Fatalf("round %d: flush without the table returned nil", round)
		}
	}
	mu.Lock()
	logged := append([]string(nil), lines...)
	mu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], "llm_usage") {
		t.Fatalf("log lines = %q, want exactly one naming sage.llm_usage", logged)
	}
	restore()
	if err := acc.Flush(ctx, sink); err != nil {
		t.Fatalf("flush after the table is back: %v", err)
	}
	f := readTotals(t, ctx, pool, database)["facts/m"]
	if f.calls != 4 || f.prompt != 4800 || f.completion != 1200 {
		t.Fatalf("facts totals = %+v, want the 4 retained calls (4800/1200)", f)
	}
}
