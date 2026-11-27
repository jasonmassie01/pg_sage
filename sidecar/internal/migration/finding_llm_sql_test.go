package migration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/rca"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// migration-10: model text never reaches recommended_sql; it lands in the
// llm_ detail keys, validated or labelled, after the deterministic finding.

const dropPkeyFallbackReply = `{"lock_level":"ACCESS EXCLUSIVE",` +
	`"requires_rewrite":false,"risk_score":0.9,` +
	`"safe_alternative":"DROP INDEX CONCURRENTLY public.users_pkey",` +
	`"explanation":"risky","estimated_duration_seconds":10}`

// analyzeEnriched is Analyze followed by the enrichment the detectors queue
// for an unclassified statement, run in line: the fallback incident the
// enricher persists (migration-10 moved the model off Analyze).
func analyzeEnriched(a *Advisor, ctx context.Context, sql string) (*rca.Incident, error) {
	inc, err := a.Analyze(ctx, sql)
	if inc != nil || err != nil {
		return inc, err
	}
	inc, _ = a.fallbackResult(ctx, sql)
	return inc, nil
}

func projectStored(t *testing.T, f MigrationSafetyFinding) cases.Case {
	t.Helper()
	raw, err := json.Marshal(f.Detail)
	if err != nil {
		t.Fatalf("marshal detail: %v", err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	return cases.ProjectFinding(cases.SourceFinding{
		ID: "mig-10", DatabaseName: f.DatabaseName, Category: SafetyFindingCategory,
		Severity: cases.Severity(f.Severity), ObjectType: f.ObjectType,
		ObjectIdentifier: f.ObjectIdentifier, RuleID: f.RuleID, Title: f.Title,
		RecommendedSQL: f.RecommendedSQL, Detail: detail, ObservedAt: time.Now(),
	}, nil) // nil classifier (cases-04): executor would be an import cycle here
}

// G7-B28 / CHECK-31: a model answering DROP INDEX CONCURRENTLY on the
// primary key yields no recommended_sql and no drop_unused_index candidate.
func TestModelDropIndexNeverBecomesARecommendation(t *testing.T) {
	rec := newRecordingLLM(t, dropPkeyFallbackReply)
	cfg := enrichCfg()
	adv := NewAdvisor(nil, cfg, 160004, "b28_db", nopMigrationLog, rec.client())
	sink := newMemorySink()
	ddls := []activeDDL{{PID: 9, Query: "ALTER TABLE users ALTER COLUMN c SET DEFAULT 'x'"}}
	d := NewDetector(nil, adv, cfg, nopMigrationLog).WithFindingSink(sink)
	d.activity = fixedActivity(&ddls)
	ctx := context.Background()
	if _, err := d.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if _, upserts, _ := sink.snapshot(); upserts != 0 {
		t.Fatalf("unclassified DDL persisted %d times before the fallback scored it", upserts)
	}
	adv.enricher.drain(ctx)
	findings, upserts, _ := sink.snapshot()
	if upserts != 1 || len(findings) != 1 || len(rec.calls()) != 1 {
		t.Fatalf("upserts = %d findings = %d calls = %d, want one scored finding",
			upserts, len(findings), len(rec.calls()))
	}
	for _, f := range findings {
		if f.RecommendedSQL != "" {
			t.Fatalf("recommended_sql = %q, want empty", f.RecommendedSQL)
		}
		if s, _ := f.Detail["safe_alternative"].(string); strings.Contains(s, "DROP") {
			t.Fatalf("detail.safe_alternative carries model text %q", s)
		}
		reason, _ := f.Detail[detailLLMInvalid].(string)
		if f.Detail[detailLLMUnvalidated] != "DROP INDEX CONCURRENTLY public.users_pkey" ||
			reason == "" || f.Detail[detailLLMSafeAlternative] != nil {
			t.Fatalf("detail = %v, want the DROP shown as unvalidated model text", f.Detail)
		}
		for _, c := range projectStored(t, f).ActionCandidates {
			if c.ActionType == "drop_unused_index" || strings.Contains(c.ProposedSQL, "DROP") {
				t.Fatalf("candidate %s proposes %q", c.ActionType, c.ProposedSQL)
			}
		}
	}
}

// A low-scoring fallback persists nothing; ineligible DDL is never sent.
func TestFallbackPersistsOnlyAboveThreshold(t *testing.T) {
	low := strings.Replace(highRiskLLMReply, `"risk_score":0.9`, `"risk_score":0.3`, 1)
	for _, tc := range []struct {
		name, reply, sql string
		calls            int
	}{
		{"at threshold", low, "ALTER TABLE t ALTER COLUMN c SET DEFAULT 'x'", 1},
		{"credential DDL", highRiskLLMReply, "ALTER ROLE app PASSWORD 'S3cretPw!'", 0},
		{"classified", highRiskLLMReply, "VACUUM FULL orders", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingLLM(t, tc.reply)
			adv := NewAdvisor(nil, enrichCfg(), 160004, "thr_db", nopMigrationLog,
				rec.client())
			sink := newMemorySink()
			adv.runEnrichJob(context.Background(), enrichJob{pid: 1, sql: tc.sql, sink: sink})
			if _, upserts, _ := sink.snapshot(); upserts != 0 {
				t.Fatalf("persisted %d findings, want none", upserts)
			}
			if got := len(rec.calls()); got != tc.calls {
				t.Fatalf("model calls = %d, want %d", got, tc.calls)
			}
		})
	}
}

func scriptJobFor(adv *Advisor, sink *memorySink) enrichJob {
	inc, risk, _ := adv.AnalyzeRisk(context.Background(), "VACUUM FULL orders")
	finding, _ := FindingFromIncident(3, "VACUUM FULL orders", inc, risk)
	_, _ = sink.UpsertMigrationSafetyFinding(context.Background(), finding)
	return enrichJob{pid: 3, sql: "VACUUM FULL orders", risk: risk,
		identity: finding.ObjectIdentifier, sink: sink}
}

// Nil/empty: an empty reply adds no model output; a markdown-wrapped
// non-script is labelled; the deterministic recommendation never moves.
func TestScriptEnrichmentEmptyAndWrappedReplies(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		wantKeys    bool
	}{
		{"empty", "   ", false},
		{"markdown prose", "```\nI cannot write that script.\n```", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecordingLLM(t, tc.reply)
			adv := NewAdvisor(nil, enrichCfg(), 160004, "empty_db", nopMigrationLog,
				rec.client())
			sink := newMemorySink()
			job := scriptJobFor(adv, sink)
			before, _, _ := sink.snapshot()
			adv.runEnrichJob(context.Background(), job)
			after, _, _ := sink.snapshot()
			f := after[job.identity]
			if f.RecommendedSQL != before[job.identity].RecommendedSQL {
				t.Fatalf("recommended_sql changed to %q", f.RecommendedSQL)
			}
			if f.Detail[detailLLMScript] != nil {
				t.Fatalf("llm_script = %v, want none", f.Detail[detailLLMScript])
			}
			if got := len(llmKeys(f.Detail)) > 0; got != tc.wantKeys {
				t.Fatalf("model keys %v, want present=%v", llmKeys(f.Detail), tc.wantKeys)
			}
			if reason, _ := f.Detail[detailLLMInvalid].(string); tc.wantKeys && reason == "" {
				t.Fatal("labelled model text has no reason")
			}
		})
	}
}

// Error propagation: a timed-out call leaves no model output and is not
// cached, so the next detection asks again.
func TestScriptEnrichmentTimeoutIsNotCached(t *testing.T) {
	slow := newSlowLLM(t, 5*time.Second, "```sql\nANALYZE orders;\n```")
	adv := NewAdvisor(nil, enrichCfg(), 160004, "timeout_db", nopMigrationLog, slow.client())
	sink := newMemorySink()
	job := scriptJobFor(adv, sink)
	for i := 1; i <= 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		adv.runEnrichJob(ctx, job)
		cancel()
		if got := slow.started.Load(); got != int64(i) {
			t.Fatalf("attempt %d: model asked %d times, want %d (no cached failure)",
				i, got, i)
		}
	}
	findings, _, merges := sink.snapshot()
	if merges != 0 || len(llmKeys(findings[job.identity].Detail)) != 0 {
		t.Fatalf("timed-out calls merged %d times: %v", merges, findings[job.identity].Detail)
	}
}

// Defence in depth: only a deterministic recommendation is copied.
func TestFindingCopiesOnlyDeterministicSQL(t *testing.T) {
	inc := migrationIncident("CREATE INDEX CONCURRENTLY idx ON t (id)")
	inc.RecommendedSQLSource = ""
	got, _ := FindingFromIncident(1, "CREATE INDEX idx ON t (id)", inc, nil)
	if got.RecommendedSQL != "" || got.Detail["safe_alternative"] != "" {
		t.Fatalf("unattributed SQL copied: sql %q detail %v", got.RecommendedSQL,
			got.Detail["safe_alternative"])
	}
	inc.RecommendedSQLSource = "model"
	got, _ = FindingFromIncident(1, "CREATE INDEX idx ON t (id)", inc, nil)
	if got.RecommendedSQL != "" {
		t.Fatalf("model SQL copied: %q", got.RecommendedSQL)
	}
	inc.RecommendedSQLSource = SQLSourceDeterministic
	got, _ = FindingFromIncident(1, "CREATE INDEX idx ON t (id)", inc, nil)
	if got.RecommendedSQL != inc.RecommendedSQL {
		t.Fatalf("deterministic SQL dropped: %q", got.RecommendedSQL)
	}
}

// The executor runs recommended_sql as one statement: a batch is refused.
func TestExecutableSafeSQLRequiresOneStatement(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		kept bool
	}{
		{"CREATE INDEX CONCURRENTLY i ON t (x)", true},
		{"CREATE INDEX CONCURRENTLY i ON t (x);", true},
		{`CREATE INDEX CONCURRENTLY "a;b" ON t (x)`, true},
		{"CREATE INDEX CONCURRENTLY i ON t (x) WHERE c = ';'", true},
		{"CREATE INDEX CONCURRENTLY i ON t (x); DROP TABLE t", false},
		{"ANALYZE t; ANALYZE u", false},
		{"", false},
	} {
		want := ""
		if tc.kept {
			want = tc.sql
		}
		if got := executableSafeSQL(tc.sql); got != want {
			t.Errorf("executableSafeSQL(%q) = %q, want %q", tc.sql, got, want)
		}
	}
}

// PostgreSQL refuses CONCURRENTLY inside a transaction block.
func TestCheckTransactionShape(t *testing.T) {
	begin := sqlast.Statement{Kind: sqlast.KindBegin}
	commit := sqlast.Statement{Kind: sqlast.KindCommit}
	cic := sqlast.Statement{Kind: sqlast.KindCreateIndex, Concurrent: true}
	plain := sqlast.Statement{Kind: sqlast.KindCreateIndex}
	for _, tc := range []struct {
		name  string
		stmts []sqlast.Statement
		bad   bool
	}{
		{"inside BEGIN/COMMIT", []sqlast.Statement{begin, cic, commit}, true},
		{"after an unclosed BEGIN", []sqlast.Statement{begin, cic}, true},
		{"after COMMIT", []sqlast.Statement{begin, plain, commit, cic}, false},
		{"alone", []sqlast.Statement{cic}, false},
		{"plain index in a block", []sqlast.Statement{begin, plain, commit}, false},
		{"empty", nil, false},
	} {
		err := checkTransactionShape(tc.stmts)
		if (err != nil) != tc.bad {
			t.Errorf("%s: err = %v, want refused=%v", tc.name, err, tc.bad)
		}
		if err != nil && !strings.Contains(err.Error(), "CONCURRENTLY") {
			t.Errorf("%s: reason %q does not name CONCURRENTLY", tc.name, err)
		}
	}
}

func TestModelFields(t *testing.T) {
	f := modelFields(ScriptResult{Script: "ANALYZE t"}, detailLLMScript)
	if f[detailLLMScript] != "ANALYZE t" || len(f) != 1 {
		t.Errorf("validated = %v", f)
	}
	f = modelFields(ScriptResult{ModelText: "GRANT x", Reason: "why"}, detailLLMSafeAlternative)
	if f[detailLLMUnvalidated] != "GRANT x" || f[detailLLMInvalid] != "why" || len(f) != 2 {
		t.Errorf("unvalidated = %v", f)
	}
	if f := modelFields(ScriptResult{SQL: "deterministic only"}, detailLLMScript); f != nil {
		t.Errorf("no model text = %v, want nil", f)
	}
}
