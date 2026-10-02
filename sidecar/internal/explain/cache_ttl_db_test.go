package explain

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

var (
	livePoolOnce sync.Once
	livePool     *pgxpool.Pool
	livePoolErr  error
)

// liveExplainPool returns a pool on this package's disposable test
// database with the sage schema bootstrapped.
func liveExplainPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	livePoolOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		livePool, livePoolErr = pgxpool.New(ctx, dsn)
		if livePoolErr == nil {
			livePoolErr = schema.Bootstrap(ctx, livePool)
		}
	})
	if livePoolErr != nil {
		t.Fatalf("live explain pool: %v", livePoolErr)
	}
	return livePool
}

func liveConfig(timeoutMs int) *config.ExplainConfig {
	return &config.ExplainConfig{Enabled: true, TimeoutMs: timeoutMs, CacheTTLMinutes: 60}
}

func cacheLifetime(t *testing.T, pool *pgxpool.Pool, query string, planOnly bool) time.Duration {
	t.Helper()
	var seconds float64
	err := pool.QueryRow(context.Background(), `SELECT extract(epoch FROM expires_at - created_at)
		FROM sage.explain_results WHERE query_hash = $1`,
		cacheKey(query, nil, planOnly)).Scan(&seconds)
	if err != nil {
		t.Fatalf("read cache row for %q: %v", query, err)
	}
	return time.Duration(seconds * float64(time.Second))
}

// Phase 0 #2: a plan-only result must never answer an ANALYZE request.
func TestCachePlanOnlyAndAnalyzeAreSeparate(t *testing.T) {
	pool := liveExplainPool(t)
	ctx := context.Background()
	q := "SELECT count(*) FROM pg_catalog.pg_class WHERE relpages >= 0 /* cache-sep */"
	_, _ = pool.Exec(ctx, "DELETE FROM sage.explain_results")
	ex := New(pool, liveConfig(5000), noopLogFn)
	planOnly, err := ex.Explain(ctx, ExplainRequest{Query: q, PlanOnly: true})
	if err != nil || planOnly.ActualTimeMs != nil {
		t.Fatalf("plan-only explain: err=%v actual=%v", err, planOnly)
	}
	analyzed, err := ex.Explain(ctx, ExplainRequest{Query: q})
	if err != nil {
		t.Fatalf("analyze explain: %v", err)
	}
	if analyzed.CachedAt != nil || analyzed.ActualTimeMs == nil {
		t.Errorf("ANALYZE request served from the plan-only cache: cached=%v actual=%v",
			analyzed.CachedAt, analyzed.ActualTimeMs)
	}
	again, err := ex.Explain(ctx, ExplainRequest{Query: q})
	if err != nil || again.CachedAt == nil || again.ActualTimeMs == nil {
		t.Errorf("second ANALYZE not served from its own cache: err=%v %+v", err, again)
	}
}

// Phase 0 #2: timeout_ms <= 0 runs with the default statement_timeout. A
// negative value used to reach SET LOCAL verbatim and fail the request.
func TestExplainNonPositiveTimeoutStillRuns(t *testing.T) {
	pool := liveExplainPool(t)
	for _, ms := range []int{0, -5} {
		ex := New(pool, liveConfig(ms), noopLogFn)
		q := "SELECT 1 AS timeout_probe_" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
		result, err := ex.Explain(context.Background(), ExplainRequest{Query: q, PlanOnly: true})
		if err != nil || result == nil {
			t.Errorf("TimeoutMs=%d: Explain = %v", ms, err)
		}
	}
}

func TestExplainPositiveTimeoutIsEnforced(t *testing.T) {
	pool := liveExplainPool(t)
	ex := New(pool, liveConfig(200), noopLogFn)
	start := time.Now()
	_, err := ex.Explain(context.Background(), ExplainRequest{
		Query: "SELECT count(*) FROM generate_series(1, 400000000) /* timeout */",
	})
	if err == nil {
		t.Fatal("a 200ms statement_timeout let a long ANALYZE finish")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
}

// Phase 0 #2: an LLM failure fallback is cached briefly; an answer, or a
// deployment without an LLM, keeps the full TTL.
func TestCacheTTLFollowsLLMOutcome(t *testing.T) {
	pool := liveExplainPool(t)
	valid := `{"summary":"ok","slow_because":[],"recommendations":[]}`
	cases := []struct {
		name    string
		status  int
		content string
		noLLM   bool
		short   bool
	}{
		{"valid", http.StatusOK, valid, false, false},
		{"fenced", http.StatusOK, "```json\n" + valid + "\n```", false, false},
		{"malformed", http.StatusOK, "not json at all", false, true},
		{"empty", http.StatusOK, "", false, true},
		{"server error", http.StatusInternalServerError, "", false, true},
		{"rate limited", http.StatusTooManyRequests, "", false, true},
		{"no llm", 0, "", true, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := "SELECT relname FROM pg_catalog.pg_class LIMIT " + string(rune('1'+i))
			_, _ = pool.Exec(context.Background(), "DELETE FROM sage.explain_results")
			ex := New(pool, liveConfig(5000), noopLogFn)
			if !c.noLLM {
				srv := newTestLLMServer(t, c.content, c.status)
				t.Cleanup(srv.Close)
				ex.llmClient = llm.New(&config.LLMConfig{Enabled: true, Endpoint: srv.URL,
					APIKey: "k", Model: "m", TimeoutSeconds: 2}, noopLogFn)
			}
			if _, err := ex.Explain(context.Background(),
				ExplainRequest{Query: q, PlanOnly: true}); err != nil {
				t.Fatalf("Explain: %v", err)
			}
			life := cacheLifetime(t, pool, q, true)
			if c.short && life > time.Duration(llmFallbackTTLMinutes)*time.Minute+time.Second {
				t.Errorf("fallback cached for %v, want <= %d min", life, llmFallbackTTLMinutes)
			}
			if !c.short && life < 59*time.Minute {
				t.Errorf("answer cached for %v, want the 60 min TTL", life)
			}
		})
	}
}
