package migration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/rca"
)

// recordingLLM is an OpenAI-compatible fake that records every request
// body it receives and replies with a fixed assistant message.
type recordingLLM struct {
	mu     sync.Mutex
	bodies []string
	reply  string
	srv    *httptest.Server
}

func newRecordingLLM(t *testing.T, reply string) *recordingLLM {
	t.Helper()
	rec := &recordingLLM{reply: reply}
	rec.srv = httptest.NewServer(http.HandlerFunc(rec.serve))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recordingLLM) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, string(body))
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{
			"message":       map[string]any{"content": r.reply},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"total_tokens": 10},
	})
}

func (r *recordingLLM) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

func (r *recordingLLM) client() *llm.Client {
	return llm.New(&config.LLMConfig{
		Enabled:          true,
		Endpoint:         r.srv.URL,
		APIKey:           "test-key",
		Model:            "test-model",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 1_000_000,
	}, nopMigrationLog)
}

const highRiskLLMReply = `{"lock_level":"ACCESS EXCLUSIVE",` +
	`"requires_rewrite":true,"risk_score":0.9,` +
	`"safe_alternative":"","explanation":"risky",` +
	`"estimated_duration_seconds":10}`

func newEgressAdvisor(rec *recordingLLM) *Advisor {
	cfg := &config.MigrationConfig{Enabled: true, Mode: "advisory"}
	return NewAdvisor(nil, cfg, 160004, "egress_db", nopMigrationLog,
		rec.client())
}

// TestLLMFallback_NeverSendsCredentialDDL is the G7-B01 regression:
// statements that can carry credentials must never reach the LLM.
func TestLLMFallback_NeverSendsCredentialDDL(t *testing.T) {
	secretDDL := []string{
		"ALTER ROLE app PASSWORD 'S3cretPw!'",
		"ALTER USER app WITH ENCRYPTED PASSWORD 'S3cretPw!'",
		"CREATE ROLE app LOGIN PASSWORD 'S3cretPw!'",
		"CREATE USER app PASSWORD 'S3cretPw!'",
		"ALTER USER MAPPING FOR app SERVER remote " +
			"OPTIONS (SET password 'S3cretPw!')",
		"CREATE USER MAPPING FOR app SERVER remote " +
			"OPTIONS (user 'u', password 'S3cretPw!')",
		"ALTER SUBSCRIPTION sub CONNECTION " +
			"'host=db password=S3cretPw! dbname=x'",
		"CREATE SUBSCRIPTION sub CONNECTION " +
			"'host=db password=S3cretPw!' PUBLICATION p",
		"ALTER SERVER remote OPTIONS (SET password 'S3cretPw!')",
		"ALTER SYSTEM SET primary_conninfo = 'password=S3cretPw!'",
		"DROP ROLE app",
		"ALTER DEFAULT PRIVILEGES GRANT SELECT ON TABLES TO app",
	}
	for _, sql := range secretDDL {
		t.Run(sql, func(t *testing.T) {
			rec := newRecordingLLM(t, highRiskLLMReply)
			inc, err := newEgressAdvisor(rec).Analyze(
				context.Background(), sql)
			if err != nil {
				t.Fatalf("Analyze error: %v", err)
			}
			if got := len(rec.calls()); got != 0 {
				t.Fatalf("LLM called %d times for %q; body=%s",
					got, sql, rec.calls()[0])
			}
			if inc != nil {
				t.Fatalf("incident produced for non-table DDL: %+v", inc)
			}
		})
	}
}

// TestLLMFallback_RedactsLiteralsInTableDDL: unclassified table DDL
// may be sent, but string literals must be redacted first (G7-B01).
func TestLLMFallback_RedactsLiteralsInTableDDL(t *testing.T) {
	cases := []string{
		"ALTER TABLE t ALTER COLUMN c SET DEFAULT 'S3cretPw!'",
		"ALTER TABLE t ALTER COLUMN c SET DEFAULT E'S3cret\\'Pw!'",
		"ALTER TABLE t ALTER COLUMN c SET DEFAULT $$S3cretPw!$$",
		"ALTER TABLE t ALTER COLUMN c SET DEFAULT $tag$S3cretPw!$tag$",
		"ALTER TABLE t ALTER COLUMN c SET DEFAULT 'it''s S3cretPw!'",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			rec := newRecordingLLM(t, highRiskLLMReply)
			inc, err := newEgressAdvisor(rec).Analyze(
				context.Background(), sql)
			if err != nil {
				t.Fatalf("Analyze error: %v", err)
			}
			calls := rec.calls()
			if len(calls) != 1 {
				t.Fatalf("LLM calls = %d, want 1", len(calls))
			}
			if strings.Contains(calls[0], "S3cret") {
				t.Fatalf("secret literal reached LLM: %s", calls[0])
			}
			if !strings.Contains(calls[0], "ALTER TABLE t") {
				t.Fatalf("statement shape missing from prompt: %s",
					calls[0])
			}
			if inc == nil {
				t.Fatal("expected LLM incident for risk 0.9")
			}
			assertIncidentHasNoSecret(t, inc, "S3cret")
		})
	}
}

func assertIncidentHasNoSecret(
	t *testing.T, inc *rca.Incident, secret string,
) {
	t.Helper()
	raw, err := json.Marshal(inc)
	if err != nil {
		t.Fatalf("marshal incident: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("incident retains secret: %s", raw)
	}
}

// TestLLMFallback_SkipsMultiStatementBatches: a table DDL prefix must
// not smuggle a second credential statement to the LLM.
func TestLLMFallback_SkipsMultiStatementBatches(t *testing.T) {
	sql := "ALTER TABLE t SET (fillfactor = 70); " +
		"ALTER ROLE app PASSWORD 'S3cretPw!'"
	rec := newRecordingLLM(t, highRiskLLMReply)
	_, err := newEgressAdvisor(rec).Analyze(context.Background(), sql)
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	for _, body := range rec.calls() {
		if strings.Contains(body, "S3cret") ||
			strings.Contains(strings.ToUpper(body), "ALTER ROLE") {
			t.Fatalf("credential statement reached LLM: %s", body)
		}
	}
}

// TestLLMFallback_SkipsProvablySafeDDL is the G7-B27 regression.
func TestLLMFallback_SkipsProvablySafeDDL(t *testing.T) {
	safe := []string{
		"CREATE INDEX CONCURRENTLY idx ON t (a)",
		"CREATE UNIQUE INDEX CONCURRENTLY idx ON t (a)",
		"ALTER TABLE t ADD CONSTRAINT c CHECK (a > 0) NOT VALID",
		"ALTER TABLE t VALIDATE CONSTRAINT c",
		"REINDEX INDEX CONCURRENTLY idx",
		"DROP INDEX CONCURRENTLY idx",
		"REFRESH MATERIALIZED VIEW CONCURRENTLY mv",
	}
	for _, sql := range safe {
		t.Run(sql, func(t *testing.T) {
			rec := newRecordingLLM(t, highRiskLLMReply)
			inc, err := newEgressAdvisor(rec).Analyze(
				context.Background(), sql)
			if err != nil {
				t.Fatalf("Analyze error: %v", err)
			}
			if got := len(rec.calls()); got != 0 {
				t.Fatalf("LLM called %d times for safe DDL", got)
			}
			if inc != nil {
				t.Fatalf("incident for provably safe DDL: %+v", inc)
			}
		})
	}
}

// TestLLMFallback_ClampsRiskAndRendersVersion covers G7-B27's
// unclamped risk_score and raw server_version_num in the prompt.
func TestLLMFallback_ClampsRiskAndRendersVersion(t *testing.T) {
	reply := strings.Replace(highRiskLLMReply,
		`"risk_score":0.9`, `"risk_score":7.5`, 1)
	rec := newRecordingLLM(t, reply)
	inc, err := newEgressAdvisor(rec).Analyze(context.Background(),
		"ALTER TABLE t ALTER COLUMN c SET STATISTICS 500")
	if err != nil {
		t.Fatalf("Analyze error: %v", err)
	}
	if inc == nil {
		t.Fatal("expected incident")
	}
	if inc.Confidence > 1.0 || inc.Confidence < 0.99 {
		t.Fatalf("Confidence = %v, want clamped to 1.0", inc.Confidence)
	}
	if inc.ActionRisk != "risk_score=1.00" {
		t.Fatalf("ActionRisk = %q, want risk_score=1.00", inc.ActionRisk)
	}
	body := rec.calls()[0]
	if strings.Contains(body, "160004") {
		t.Fatalf("prompt contains raw version number: %s", body)
	}
	if !strings.Contains(body, "PostgreSQL version: 16.4") {
		t.Fatalf("prompt lacks major.minor version: %s", body)
	}
}

// TestFindingFromIncident_RedactsLiterals: persisted migration
// findings must not store observed DDL literals (G7-B01).
func TestFindingFromIncident_RedactsLiterals(t *testing.T) {
	sql := "ALTER TABLE t ADD CONSTRAINT c CHECK (pw <> 'S3cretPw!')"
	inc := &rca.Incident{
		Severity: "warning", Source: "schema_advisor",
		RootCause:       "Dangerous DDL: ddl_constraint_not_valid",
		SignalIDs:       []string{"ddl_constraint_not_valid"},
		AffectedObjects: []string{"public.t"},
		CausalChain:     []rca.ChainLink{{Order: 1, Evidence: sql}},
		DatabaseName:    "db",
	}
	finding, ok := FindingFromIncident(42, sql, inc)
	if !ok {
		t.Fatal("FindingFromIncident returned ok=false")
	}
	raw, err := json.Marshal(finding)
	if err != nil {
		t.Fatalf("marshal finding: %v", err)
	}
	if strings.Contains(string(raw), "S3cret") {
		t.Fatalf("persisted finding retains secret: %s", raw)
	}
	if !strings.Contains(finding.OriginalSQL, "'***'") {
		t.Fatalf("OriginalSQL = %q, want redacted literal marker",
			finding.OriginalSQL)
	}
}

// TestScriptGenerator_RedactsLiterals: the classified path's LLM
// script generator must not ship literals either (G7-B01).
func TestScriptGenerator_RedactsLiterals(t *testing.T) {
	rec := newRecordingLLM(t, "```sql\nSELECT 1;\n```")
	gen := NewScriptGenerator(rec.client(), nil, 160004, nopMigrationLog)
	risk := &DDLRisk{
		Statement: "ALTER TABLE t ADD CONSTRAINT c " +
			"CHECK (pw <> 'S3cretPw!')",
		RuleID: "ddl_constraint_not_valid", TableName: "t",
	}
	if _, err := gen.Generate(context.Background(), risk); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("LLM calls = %d, want 1", len(calls))
	}
	if strings.Contains(calls[0], "S3cret") {
		t.Fatalf("secret reached script LLM: %s", calls[0])
	}
	if strings.Contains(calls[0], "160004") {
		t.Fatalf("script prompt contains raw version number")
	}
}
