package safetybench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Environment variables that gate and configure the bench, mirroring
// sre-bench's conventions.
const (
	// EnvRun must be "1" for TestAgentSafetyBench to run the full bench.
	EnvRun = "SAGE_SAFETY_BENCH_RUN"
	// EnvReportDir is where the JSON and Markdown report are written.
	EnvReportDir = "SAGE_SAFETY_BENCH_REPORT_DIR"
	// EnvPgSageVersion and EnvPgSageCommit stamp the report with the build.
	EnvPgSageVersion = "SAGE_BENCH_PG_SAGE_VERSION"
	EnvPgSageCommit  = "SAGE_BENCH_PG_SAGE_COMMIT"

	reportSchema   = "agentsafetybench"
	schemaRevision = 1
	reportJSON     = "agentsafetybench.json"
	reportMarkdown = "agentsafetybench.md"
)

// Report is the machine-readable AgentSafetyBench result.
type Report struct {
	Schema         string                `json:"schema"`
	SchemaRevision int                   `json:"schema_revision"`
	GeneratedAt    time.Time             `json:"generated_at"`
	ServerVersion  string                `json:"server_version"`
	PgSageVersion  string                `json:"pg_sage_version,omitempty"`
	PgSageCommit   string                `json:"pg_sage_commit,omitempty"`
	Designs        []string              `json:"readonly_designs"`
	ReadOnly       []CaseResult          `json:"readonly"`
	Posture        []PostureResult       `json:"posture"`
	PostureWired   bool                  `json:"posture_wired"`
	Incidents      []IncidentExpectation `json:"incidents"`
	IncidentScore  IncidentScore         `json:"incident_score"`
}

// ReportMeta carries the run metadata the report stamps.
type ReportMeta struct {
	ServerVersion string
	PgSageVersion string
	PgSageCommit  string
	Designs       []string
	PostureWired  bool
}

// BuildReport assembles a Report from the three sections.
func BuildReport(meta ReportMeta, ro []CaseResult, posture []PostureResult,
	incidents []IncidentExpectation) Report {
	return Report{
		Schema:         reportSchema,
		SchemaRevision: schemaRevision,
		GeneratedAt:    time.Now().UTC(),
		ServerVersion:  meta.ServerVersion,
		PgSageVersion:  meta.PgSageVersion,
		PgSageCommit:   meta.PgSageCommit,
		Designs:        meta.Designs,
		ReadOnly:       ro,
		Posture:        posture,
		PostureWired:   meta.PostureWired,
		Incidents:      incidents,
		IncidentScore:  ScoreIncidents(incidents),
	}
}

// ReadOnlySummary counts, per design, how many cases the design held.
func (r Report) ReadOnlySummary() map[string]struct{ Held, Total int } {
	out := map[string]struct{ Held, Total int }{}
	for _, c := range r.ReadOnly {
		for _, a := range c.Attempts {
			s := out[a.Design]
			s.Total++
			if a.Held() {
				s.Held++
			}
			out[a.Design] = s
		}
	}
	return out
}

// WriteReport writes the JSON and Markdown report into dir.
func WriteReport(dir string, r Report) (jsonPath, mdPath string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("report directory %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode report: %w", err)
	}
	jsonPath = filepath.Join(dir, reportJSON)
	mdPath = filepath.Join(dir, reportMarkdown)
	if err := os.WriteFile(jsonPath, append(raw, '\n'), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", jsonPath, err)
	}
	if err := os.WriteFile(mdPath, []byte(r.Markdown()), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", mdPath, err)
	}
	return jsonPath, mdPath, nil
}

// ReportDir returns v, or fallback when v is empty.
func ReportDir(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// BenchRunRequested reports whether EnvRun asks for the full bench.
func BenchRunRequested(getenv func(string) string) bool {
	return getenv(EnvRun) == "1"
}

// BuildFromEnv reads the pg_sage build stamp from the environment.
func BuildFromEnv(getenv func(string) string) (version, commit string) {
	return getenv(EnvPgSageVersion), getenv(EnvPgSageCommit)
}
