package srebench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The bench writes a machine-readable JSON result and a Markdown summary
// (per family x arm) to SAGE_BENCH_REPORT_DIR; repeat counts come from
// SAGE_BENCH_REPEATS.

func sampleReport() Report {
	rs := append(healthy(), Result{Scenario: Scenario{ID: "broken", Family: sre.TriggerWAL,
		Class: ClassPositive, Gold: Gold{Root: "inactive_slot"}}, Arm: armA, Repeat: 1,
		Attempts: 1, Err: errTest})
	rs = append(rs, family(armA, sre.TriggerLock, ClassPositive, Gold{Root: "ddl_lock_queue"},
		4, 5, "hot_row_contention")...)
	return BuildReport(rs, ReportMeta{Arms: []string{armA, ArmLLM}, Gated: []string{armA},
		Pending: map[string]string{ArmLLM: "model turn not wired"}, Repeats: 1,
		ServerVersion: "PostgreSQL 16.14", GeneratedAt: time.Unix(1_800_000_000, 0).UTC(),
		LLM: LLMConfig{Mode: LLMFake}})
}

func TestBuildReport_JSONHandlesZeroDenominators(t *testing.T) {
	r := sampleReport()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal (NaN must never reach JSON): %v", err)
	}
	var doc struct {
		Schema string `json:"schema"`
		Cells  []struct {
			Arm    string `json:"arm"`
			Family string `json:"family"`
			Top1   struct {
				K    int      `json:"k"`
				N    int      `json:"n"`
				Rate *float64 `json:"rate"`
				Low  *float64 `json:"wilson_low"`
			} `json:"top1"`
			Errored int `json:"errored"`
		} `json:"cells"`
		Gates []GateResult `json:"gates"`
		Runs  []struct {
			Scenario string `json:"scenario"`
			Error    string `json:"error"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Schema != ReportSchema || len(doc.Runs) != len(healthy())+1+5 {
		t.Fatalf("schema %q, %d runs", doc.Schema, len(doc.Runs))
	}
	var wal, lock bool
	for _, c := range doc.Cells {
		switch {
		case c.Arm == armA && c.Family == string(sre.TriggerWAL):
			wal = c.Top1.N == 0 && c.Top1.Rate == nil && c.Top1.Low == nil && c.Errored == 1
		case c.Arm == armA && c.Family == lockFam:
			lock = c.Top1.K == 14 && c.Top1.N == 15 && c.Top1.Rate != nil &&
				near(*c.Top1.Rate, 14.0/15)
		}
	}
	if !wal || !lock {
		t.Fatalf("cells: wal ok=%v lock ok=%v\n%s", wal, lock, raw)
	}
}

func TestBuildReport_PendingArmIsListedAndNotEvaluated(t *testing.T) {
	r := sampleReport()
	if len(r.Arms) != 2 || r.Arms[1] != ArmLLM || r.Pending[ArmLLM] == "" {
		t.Fatalf("arms %v pending %v", r.Arms, r.Pending)
	}
	llmGates := 0
	for _, g := range r.Gates {
		if g.Arm == ArmLLM {
			llmGates++
			if g.Status != GateNotEvaluated || !strings.Contains(g.Reason, "not wired") {
				t.Fatalf("LLM gate %+v", g)
			}
		}
	}
	if llmGates == 0 {
		t.Fatal("the LLM-on arm has no gates in the report")
	}
	md := r.Markdown()
	if !strings.Contains(md, ArmLLM) || !strings.Contains(md, "not evaluated: model turn "+
		"not wired") {
		t.Fatalf("markdown does not list the pending arm:\n%s", md)
	}
}

func TestReport_MarkdownTables(t *testing.T) {
	md := sampleReport().Markdown()
	for _, want := range []string{
		"| family | arm | runs |",
		"| " + lockFam + " | " + armA + " |",
		"93% (14/15) [70-99]", // lock top-1 with its Wilson interval
		"n/a",                 // wal top-1 has no scored runs
		"| " + GateTop1 + " | " + lockFam + " | " + armA + " | pass |",
		"not_evaluated",
		"error: " + errTest.Error(),
		"PostgreSQL 16.14",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "|") && !strings.HasSuffix(line, "|") {
			t.Errorf("table row is not closed: %q", line)
		}
	}
}

func TestWriteReport_WritesBothFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "report")
	r := sampleReport()
	jsonPath, mdPath, err := WriteReport(dir, r)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read json: %v", err)
	}
	var back Report
	if err := json.Unmarshal(raw, &back); err != nil || back.Schema != ReportSchema ||
		len(back.Runs) != len(r.Runs) {
		t.Fatalf("json round trip: %v (%d runs)", err, len(back.Runs))
	}
	md, err := os.ReadFile(mdPath)
	if err != nil || string(md) != r.Markdown() {
		t.Fatalf("markdown file: %v", err)
	}
	if filepath.Dir(jsonPath) != dir || filepath.Dir(mdPath) != dir {
		t.Fatalf("paths %s %s not under %s", jsonPath, mdPath, dir)
	}
}

func TestWriteReport_DirectoryIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := WriteReport(file, sampleReport())
	if err == nil || !strings.Contains(err.Error(), "report directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseRepeats(t *testing.T) {
	good := map[string]int{"": 1, "1": 1, "3": 3, " 2 ": 2, "10": MaxRepeats}
	for in, want := range good {
		if got, err := ParseRepeats(in); err != nil || got != want {
			t.Errorf("ParseRepeats(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"0", "-1", "abc", "11", "2.5"} {
		if got, err := ParseRepeats(in); err == nil || !strings.Contains(err.Error(),
			EnvRepeats) {
			t.Errorf("ParseRepeats(%q) = %d, %v; want an error naming %s", in, got, err,
				EnvRepeats)
		}
	}
}

func TestReportDir(t *testing.T) {
	if got := ReportDir("", "/tmp/fallback"); got != "/tmp/fallback" {
		t.Fatalf("empty: %q", got)
	}
	if got := ReportDir("  /out/bench ", "/tmp/fallback"); got != "/out/bench" {
		t.Fatalf("set: %q", got)
	}
}
