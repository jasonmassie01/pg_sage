package migration

import (
	"context"
	"testing"
)

// migration-10 review: the reply cache keeps only a usable answer. A reply
// pg_sage cannot use (a fallback reply that does not parse, a script reply
// with no SQL) is a failed call, so the next detection asks again instead
// of going without model output for the cache's 24 hours.

// No concurrent access tests: each case runs one advisor on one goroutine;
// the cache's own concurrency is covered in enrich_cache_test.go.

const fallbackSQL = "ALTER TABLE t ALTER COLUMN c SET DEFAULT 'x'"

func TestFallbackReplyCachedOnlyWhenItParses(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		wantCalls   int
		wantInc     bool
	}{
		{"usable reply is cached", highRiskLLMReply, 1, true},
		{"malformed JSON is asked again", `{"lock_level": "ACCESS`, 2, false},
		{"empty reply is asked again", "   ", 2, false},
		{"no explanation is asked again", `{"risk_score":0.9}`, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingLLM(t, tc.reply)
			adv := NewAdvisor(nil, enrichCfg(), 160004, "retry_db", nopMigrationLog,
				rec.client())
			for i := 0; i < 2; i++ {
				inc, _ := adv.fallbackResult(context.Background(), fallbackSQL)
				if (inc != nil) != tc.wantInc {
					t.Fatalf("detection %d: incident = %v, want present=%v",
						i+1, inc, tc.wantInc)
				}
			}
			if got := len(rec.calls()); got != tc.wantCalls {
				t.Fatalf("model calls = %d over two detections, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestScriptReplyCachedOnlyWhenItHoldsSQL(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		wantCalls   int
	}{
		{"script is cached", "```sql\nANALYZE orders;\n```", 1},
		{"blank reply is asked again", "  \n ", 2},
		{"empty fence is asked again", "```sql\n```", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingLLM(t, tc.reply)
			adv := NewAdvisor(nil, enrichCfg(), 160004, "retry_db", nopMigrationLog,
				rec.client())
			sink := newMemorySink()
			job := scriptJobFor(adv, sink)
			before, _, _ := sink.snapshot()
			for i := 0; i < 2; i++ {
				adv.runEnrichJob(context.Background(), job)
			}
			if got := len(rec.calls()); got != tc.wantCalls {
				t.Fatalf("model calls = %d over two detections, want %d", got, tc.wantCalls)
			}
			after, _, _ := sink.snapshot()
			if got, want := after[job.identity].RecommendedSQL,
				before[job.identity].RecommendedSQL; got != want {
				t.Fatalf("recommended_sql = %q after enrichment, want %q", got, want)
			}
		})
	}
}
