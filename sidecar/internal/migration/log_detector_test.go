package migration

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
)

// ---------------------------------------------------------------
// isDDLKeyword — pure function, fully deterministic.
// ---------------------------------------------------------------

func TestIsDDLKeyword_Matches(t *testing.T) {
	cases := []string{
		"ALTER TABLE foo ADD COLUMN bar int",
		"  alter table foo drop column bar", // lower-case + leading ws
		"CREATE INDEX idx_foo ON foo(bar)",
		"DROP TABLE foo",
		"REINDEX TABLE foo",
		"VACUUM FULL foo",
		"REFRESH MATERIALIZED VIEW mv1",
		"CLUSTER foo USING idx_foo",
	}
	for _, sql := range cases {
		if !isDDLKeyword(sql) {
			t.Errorf("isDDLKeyword(%q) = false, want true", sql)
		}
	}
}

func TestIsDDLKeyword_Rejects(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"SELECT 1",
		"INSERT INTO foo VALUES (1)",
		"UPDATE foo SET bar = 2",
		"DELETE FROM foo",
		"BEGIN",
		"-- ALTER TABLE in a comment",
	}
	for _, sql := range cases {
		if isDDLKeyword(sql) {
			t.Errorf("isDDLKeyword(%q) = true, want false", sql)
		}
	}
}

// ---------------------------------------------------------------
// extractDDLFromEntry — the Query field takes precedence, then the
// Message field is used as a fallback.
// ---------------------------------------------------------------

func TestExtractDDLFromEntry_PrefersQueryField(t *testing.T) {
	entry := logwatch.LogEntry{
		Query:   "ALTER TABLE foo ADD COLUMN bar int",
		Message: "SELECT 1", // non-DDL — must be ignored
	}
	got := extractDDLFromEntry(entry)
	if got != entry.Query {
		t.Errorf("got %q, want %q", got, entry.Query)
	}
}

func TestExtractDDLFromEntry_FallsBackToMessage(t *testing.T) {
	entry := logwatch.LogEntry{
		Query:   "",
		Message: "CREATE INDEX idx_foo ON foo(bar)",
	}
	got := extractDDLFromEntry(entry)
	if got != entry.Message {
		t.Errorf("got %q, want %q", got, entry.Message)
	}
}

func TestExtractDDLFromEntry_ReturnsEmptyForNonDDL(t *testing.T) {
	entry := logwatch.LogEntry{
		Query:   "SELECT 1",
		Message: "some log line",
	}
	if got := extractDDLFromEntry(entry); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

// ---------------------------------------------------------------
// NewLogDetector + ProcessLogEntry — wire-up test. We use a real
// Advisor with a nil LLM client (deterministic-only classifier).
// A dangerous DDL must produce a non-nil Incident; non-DDL must
// return nil with no errors.
// ---------------------------------------------------------------

func TestNewLogDetector_Roundtrip(t *testing.T) {
	var logs []string
	logFn := func(level, msg string, args ...any) {
		logs = append(logs, level+": "+msg)
	}

	advisor := NewAdvisor(
		nil,                           // pool — risk assessor tolerates nil
		&config.MigrationConfig{Mode: "advisory"},
		160000,
		"testdb",
		logFn,
		nil, // llmClient — no LLM, pure deterministic path
	)

	det := NewLogDetector(advisor, logFn)
	if det == nil {
		t.Fatal("NewLogDetector returned nil")
	}

	// Non-DDL: must return nil without touching the Advisor.
	inc := det.ProcessLogEntry(logwatch.LogEntry{Message: "SELECT 1"})
	if inc != nil {
		t.Errorf("non-DDL entry produced incident: %+v", inc)
	}

	// DDL: the classifier/assessor path will run. With a nil pool the
	// risk assessor's active-query/lag queries will fail gracefully
	// (logged as warnings). The deterministic rule itself still matches,
	// so the outcome depends on rule severity. We assert the function
	// returns without panicking and, if it logs anything, it does not
	// log at "error" level.
	_ = det.ProcessLogEntry(logwatch.LogEntry{
		Message: "ALTER TABLE foo ADD COLUMN bar int NOT NULL DEFAULT 0",
	})
	for _, l := range logs {
		if len(l) >= 6 && l[:6] == "error:" {
			t.Errorf("unexpected error-level log: %s", l)
		}
	}
}

// ---------------------------------------------------------------
// buildLLMIncident — constructs an incident from a pre-parsed LLM
// response. Exercised without a live LLM.
// ---------------------------------------------------------------

func TestBuildLLMIncident_HighRiskScoresCritical(t *testing.T) {
	advisor := NewAdvisor(
		nil,
		&config.MigrationConfig{Mode: "advisory"},
		160000,
		"mydb",
		func(string, string, ...any) {},
		nil,
	)

	resp := &llmDDLResponse{
		LockLevel:       "ACCESS EXCLUSIVE",
		RequiresRewrite: true,
		RiskScore:       0.9,
		SafeAlternative: "ALTER TABLE foo ADD COLUMN bar int; UPDATE ...",
		Explanation:     "Full rewrite on a 1TB table",
		EstDurationSec:  900,
	}

	inc := advisor.buildLLMIncident(resp, "ALTER TABLE foo ALTER COLUMN id TYPE bigint")
	if inc == nil {
		t.Fatal("buildLLMIncident returned nil")
	}
	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want 'critical' for risk_score=0.9",
			inc.Severity)
	}
	if inc.Source != "schema_advisor_llm" {
		t.Errorf("Source = %q, want 'schema_advisor_llm'", inc.Source)
	}
	if inc.Confidence != 0.9 {
		t.Errorf("Confidence = %f, want 0.9", inc.Confidence)
	}
	if inc.RecommendedSQL != resp.SafeAlternative {
		t.Errorf("RecommendedSQL not propagated: got %q", inc.RecommendedSQL)
	}
	if inc.DatabaseName != "mydb" {
		t.Errorf("DatabaseName = %q, want 'mydb'", inc.DatabaseName)
	}
	// With a safe alternative present, the causal chain has 3 links:
	// llm_ddl_analysis, lock_analysis, safe_alternative.
	if len(inc.CausalChain) != 3 {
		t.Errorf("CausalChain len = %d, want 3", len(inc.CausalChain))
	}
}

func TestBuildLLMIncident_MediumRiskIsWarning(t *testing.T) {
	advisor := NewAdvisor(
		nil,
		&config.MigrationConfig{Mode: "advisory"},
		160000,
		"db1",
		func(string, string, ...any) {},
		nil,
	)
	resp := &llmDDLResponse{
		LockLevel:       "SHARE",
		RequiresRewrite: false,
		RiskScore:       0.5,
		SafeAlternative: "", // no alternative
		Explanation:     "Moderate risk",
		EstDurationSec:  10,
	}
	inc := advisor.buildLLMIncident(resp, "ALTER TABLE t ADD COLUMN x int")
	if inc.Severity != "warning" {
		t.Errorf("Severity = %q, want 'warning' for risk_score=0.5",
			inc.Severity)
	}
	// Without a safe alternative, the causal chain has exactly 2 links.
	if len(inc.CausalChain) != 2 {
		t.Errorf("CausalChain len = %d, want 2 (no safe_alternative link)",
			len(inc.CausalChain))
	}
}
