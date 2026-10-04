package srebench

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/sre-bench/replay"
)

// Roadmap 2.4: the bench report schema gains a revision (2) and the
// held-out model lift, additively: "schema" stays
// pg_sage.pgincidentbench.v1, so a v1.9.0 sidecar still ingests a new
// report (ignoring the lift) and this sidecar still ingests a v1.9.0
// report (with no lift). The producer and the consumer are tested
// against each other.

func liftReplay(t *testing.T) *ReplayReport {
	t.Helper()
	rs := overrideMix()
	cases := []replay.Case{{ID: "c1", Family: "lock_blocking", Class: replay.ClassPositive}}
	rep := BuildReplayReport(rs, cases, ReplayMeta{Arms: []string{ArmCausalGraph, ArmLLM},
		Gated: []string{ArmCausalGraph, ArmLLM}, LLM: LLMConfig{Mode: LLMLive, Model: "m"},
		GeneratedAt: time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)})
	return &rep
}

func TestReport_SchemaRevisionAndModelLift(t *testing.T) {
	r := BuildReport(nil, ReportMeta{Arms: []string{ArmCausalGraph, ArmLLM},
		Gated: []string{ArmCausalGraph, ArmLLM}, GeneratedAt: time.Now().UTC(),
		LLM: LLMConfig{Mode: LLMLive, Model: "m"}})
	if r.Schema != ReportSchema || ReportSchema != "pg_sage.pgincidentbench.v1" ||
		r.SchemaRevision != ReportSchemaRevision || ReportSchemaRevision != 2 {
		t.Fatalf("schema %q revision %d", r.Schema, r.SchemaRevision)
	}
	if len(r.ModelLift) != 0 {
		t.Fatal("a report without the replay has no model lift")
	}
	r.AttachReplay(liftReplay(t))
	if r.Replay == nil || len(r.ModelLift) == 0 ||
		len(r.ModelLift) != len(r.Replay.ModelLift) {
		t.Fatalf("attached lift = %+v", r.ModelLift)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"schema":"pg_sage.pgincidentbench.v1"`,
		`"schema_revision":2`, `"model_lift":[`, `"override_precision":`,
		`"inconclusive_lift":`, `"override_rule":`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("report JSON lacks %s", key)
		}
	}
	r.AttachReplay(nil)
	if r.Replay != nil || r.ModelLift != nil {
		t.Fatal("detaching the replay must drop its lift")
	}
}

func TestReport_TheLedgerReadsTheLiftTheBenchWrites(t *testing.T) {
	r := BuildReport(nil, ReportMeta{Arms: []string{ArmCausalGraph, ArmLLM},
		Gated: []string{ArmCausalGraph, ArmLLM}, LLM: LLMConfig{Mode: LLMLive, Model: "m"},
		GeneratedAt: time.Now().UTC().Add(-time.Minute)})
	r.AttachReplay(liftReplay(t))
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	run, err := earned.ParseBenchReport(raw, time.Now())
	if err != nil {
		t.Fatalf("the ledger refused the bench's report: %v", err)
	}
	if run.SchemaRevision != 2 || run.ModelLift == nil || run.ModelLift.Mode != LLMLive ||
		len(run.ModelLift.Records) != len(r.ModelLift) {
		t.Fatalf("ingested lift = %+v", run.ModelLift)
	}
	got := run.ModelLift.Records[0]
	want := r.ModelLift[0]
	if got.Family != want.Family || got.Arm != want.Arm || got.Split != want.Split ||
		got.Overrides.K != want.Overrides.K || got.Overrides.N != want.Overrides.N ||
		got.OverrideSafePass.K != want.OverrideSafePass.K ||
		got.ResolvedRight != want.ResolvedRight || got.Inconclusive != want.Inconclusive {
		t.Fatalf("ledger %+v vs bench %+v", got, want)
	}
}

func TestReport_WithoutReplayIsAV190ShapedReport(t *testing.T) {
	rs := as(ArmCausalGraph, healthy())
	r := BuildReport(rs, ReportMeta{Arms: []string{ArmCausalGraph},
		Gated: []string{ArmCausalGraph}, GeneratedAt: time.Now().UTC().Add(-time.Minute)})
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"model_lift"`) {
		t.Fatal("a report without held-out replay must not carry an empty model_lift")
	}
	run, err := earned.ParseBenchReport(raw, time.Now())
	if err != nil || run.ModelLift != nil || len(run.Cells) == 0 {
		t.Fatalf("ingested = %+v (%v)", run, err)
	}
}

func TestReport_MarkdownShowsModelLift(t *testing.T) {
	r := BuildReport(nil, ReportMeta{Arms: []string{ArmCausalGraph, ArmLLM},
		Gated: []string{ArmCausalGraph, ArmLLM}, GeneratedAt: time.Now().UTC()})
	r.AttachReplay(liftReplay(t))
	md := r.Markdown()
	for _, want := range []string{"Model lift over deterministic", "held-out",
		"override precision", "3/5", "inconclusive", "advisory"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q", want)
		}
	}
}

func TestReplayReport_HeldOutSection(t *testing.T) {
	rep := liftReplay(t)
	if len(rep.HeldOutCells) == 0 || rep.HeldOutCases != 10 {
		t.Fatalf("held-out cells %d cases %d", len(rep.HeldOutCells), rep.HeldOutCases)
	}
	for _, c := range rep.HeldOutCells {
		if c.Family == lockFam && c.Arm == ArmLLM && c.Runs != 10 {
			t.Fatalf("held-out cell = %+v", c)
		}
	}
	md := rep.Markdown()
	if !strings.Contains(md, "Held-out") || !strings.Contains(md, "Model lift") {
		t.Fatalf("replay markdown lacks the held-out section:\n%s", md)
	}
	tuned := BuildReplayReport(withSplit(overrideMix(), replay.SplitTuning), nil, ReplayMeta{
		Arms: []string{ArmCausalGraph, ArmLLM}, LLM: LLMConfig{Mode: LLMFake}})
	if tuned.HeldOutCases != 0 || len(tuned.ModelLift) != 0 {
		t.Fatalf("a tuning-only replay = %d held-out, lift %+v", tuned.HeldOutCases,
			tuned.ModelLift)
	}
}

func TestModelStats_DisagreementPayload(t *testing.T) {
	m := &ModelStats{}
	for _, ev := range []sre.Event{
		{Type: sre.EventModelReviewed, Payload: json.RawMessage(`{"turns": 1}`)},
		{Type: sre.EventModelDisagreed, Payload: json.RawMessage(`{"graph_root":
			"idle_in_tx_holder", "model_root": "ddl_lock_queue", "authority": "advisory"}`)},
		{Type: sre.EventModelRejected, Payload: json.RawMessage(`not json`)},
	} {
		countModelEvent(m, ev)
	}
	if m.Reviewed != 1 || m.Disagreed != 1 || m.Rejected != 1 ||
		m.GraphRoot != "idle_in_tx_holder" || m.ModelRoot != "ddl_lock_queue" ||
		m.Authority != sre.ContestAdvisory {
		t.Fatalf("stats = %+v", m)
	}
	broken := &ModelStats{}
	countModelEvent(broken, sre.Event{Type: sre.EventModelDisagreed,
		Payload: json.RawMessage(`{`)})
	if broken.Disagreed != 1 || broken.ModelRoot != "" {
		t.Fatalf("a malformed payload still counts, without a root: %+v", broken)
	}
}
