package safetybench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleReport() Report {
	meta := ReportMeta{ServerVersion: "PostgreSQL 17", Designs: []string{"read_only_txn",
		"privilege_role", "explain_guard"}}
	ro := []CaseResult{{
		ID: "sc-insert", Technique: "benign insert", Expect: ClassPrivilege,
		SelfCheck: true,
		Attempts: []Attempt{
			{Design: "read_only_txn", Observed: ClassReadOnly, ChecksumsIntact: true},
			{Design: "privilege_role", Observed: ClassPrivilege, ChecksumsIntact: true},
			{Design: "explain_guard", Observed: ClassBeforeExecution, ChecksumsIntact: true},
		},
	}}
	posture := []PostureResult{{ID: "PS-x", Name: "exposed table", Expect: []string{"AP-03"},
		Provider: "not_connected", Connected: false, MissingDetectors: []string{"AP-03"}}}
	return BuildReport(meta, ro, posture, IncidentMapping())
}

func TestReadOnlySummary(t *testing.T) {
	r := sampleReport()
	sum := r.ReadOnlySummary()
	for _, d := range r.Designs {
		s := sum[d]
		if s.Total != 1 || s.Held != 1 {
			t.Errorf("design %s: held %d/%d, want 1/1", d, s.Held, s.Total)
		}
	}
}

func TestMarkdownSections(t *testing.T) {
	md := sampleReport().Markdown()
	for _, want := range []string{"# AgentSafetyBench v0", "## Read-only designs",
		"## Posture scenarios", "## Incident-to-control mapping",
		"Detector framework not connected", "held (privilege_error)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
}

func TestMarkdownFlagsNotHeld(t *testing.T) {
	r := sampleReport()
	r.ReadOnly[0].Attempts[0] = Attempt{Design: "read_only_txn",
		Observed: ClassExecuted, ChecksumsIntact: true}
	if !strings.Contains(r.Markdown(), "NOT HELD") {
		t.Error("an executed attempt must render as NOT HELD")
	}
}

func TestWriteReport(t *testing.T) {
	dir := t.TempDir()
	jsonPath, mdPath, err := WriteReport(dir, sampleReport())
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if jsonPath != filepath.Join(dir, reportJSON) {
		t.Errorf("json path = %s", jsonPath)
	}
	for _, p := range []string{jsonPath, mdPath} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() == 0 {
			t.Errorf("%s is empty", p)
		}
	}
}

func TestReportDirFallback(t *testing.T) {
	if ReportDir("", "fallback") != "fallback" {
		t.Error("empty must use fallback")
	}
	if ReportDir("set", "fallback") != "set" {
		t.Error("set value must win")
	}
}

func TestBenchRunRequested(t *testing.T) {
	if BenchRunRequested(func(string) string { return "1" }) != true {
		t.Error(`"1" must request a run`)
	}
	if BenchRunRequested(func(string) string { return "" }) {
		t.Error("empty must not request a run")
	}
}
