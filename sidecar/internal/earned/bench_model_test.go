package earned

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/modellift"
)

// llm-01: a revision-2 report's llm.model is an authority input. The
// ledger keeps the measured model (normalized) with the model lift, and
// a family's root authority holds only for an investigator running that
// model. A report that names no model grants nothing.

// No concurrent access tests: ParseBenchReport and rootAuthorityOf are
// pure functions of their arguments (the store's selection is tested on
// PostgreSQL in store_model_lift_db_test.go).

// fixtureModel is the model liftReport's reports were measured with.
const fixtureModel = "gpt-4o-mini"

// liftReportModel is liftReport measured with model ("": no llm.model).
func liftReportModel(at time.Time, model string, recs ...string) []byte {
	raw := string(liftReport(at, "live", false, recs...))
	field := `, "model": "` + fixtureModel + `"`
	if model == "" {
		return []byte(strings.Replace(raw, field, "", 1))
	}
	quoted, _ := json.Marshal(model)
	return []byte(strings.Replace(raw, field, `, "model": `+string(quoted), 1))
}

func TestParseBenchReport_KeepsTheMeasuredModelNormalized(t *testing.T) {
	for raw, want := range map[string]string{
		fixtureModel:          fixtureModel,
		"OpenAI/GPT-4o-mini ": fixtureModel,
		"  llama3.1:8b":       "llama3.1:8b",
	} {
		run, err := ParseBenchReport(liftReportModel(benchNow.Add(-time.Hour), raw,
			good("lock_blocking", 16, 16)), benchNow)
		if err != nil {
			t.Fatalf("%q: parse: %v", raw, err)
		}
		if run.ModelLift == nil || run.ModelLift.Model != want {
			t.Errorf("%q: model lift %+v, want model %q", raw, run.ModelLift, want)
		}
	}
}

func TestParseBenchReport_RevisionTwoWithoutModelIngestsAndGrantsNothing(t *testing.T) {
	raw := liftReportModel(benchNow.Add(-time.Hour), "", good("lock_blocking", 40, 40))
	if strings.Contains(string(raw), `"model"`) {
		t.Fatalf("fixture still names a model: %s", raw)
	}
	run, err := ParseBenchReport(raw, benchNow)
	if err != nil {
		t.Fatalf("a revision-2 report without llm.model must still ingest: %v", err)
	}
	if run.ModelLift == nil || run.ModelLift.Model != "" || len(run.ModelLift.Records) != 1 {
		t.Fatalf("model lift = %+v, want one record and no model", run.ModelLift)
	}
	got := rootAuthorityOf(&run, FamilyLockBlocking, benchNow,
		DefaultThresholds().BenchMaxAge, fixtureModel)
	if got.Granted || got.Status != RootAdvisory ||
		!strings.Contains(got.Reason, "the measurement names no model") {
		t.Fatalf("authority = %+v, want advisory naming the missing model", got)
	}
	if got.Lift == nil || got.Report == nil || got.MeasuredModel != "" ||
		got.RunningModel != fixtureModel {
		t.Fatalf("the advisory answer must still carry the measurement: %+v", got)
	}
}

func TestParseBenchReport_RejectsAnUnreadableModel(t *testing.T) {
	for name, model := range map[string]string{
		"too long":     strings.Repeat("m", maxModelBytes+1),
		"newline":      "gpt-4o\nmini",
		"control char": "gpt-4o\x00mini",
	} {
		raw := liftReportModel(benchNow.Add(-time.Hour), model, good("lock_blocking", 16, 16))
		if _, err := ParseBenchReport(raw, benchNow); !errors.Is(err, ErrInvalidReport) ||
			!strings.Contains(err.Error(), "llm.model") {
			t.Errorf("%s: err = %v, want an invalid report naming llm.model", name, err)
		}
	}
	at := liftReportModel(benchNow.Add(-time.Hour), strings.Repeat("m", maxModelBytes),
		good("lock_blocking", 16, 16))
	if _, err := ParseBenchReport(at, benchNow); err != nil {
		t.Fatalf("a model of exactly %d bytes must parse: %v", maxModelBytes, err)
	}
}

func TestRootAuthority_BoundToTheMeasuredModel(t *testing.T) {
	raw := liftReportModel(benchNow.Add(-time.Hour), "gpt-6", good("lock_blocking", 16, 16))
	run, err := ParseBenchReport(raw, benchNow)
	if err != nil {
		t.Fatal(err)
	}
	maxAge := DefaultThresholds().BenchMaxAge
	for running, want := range map[string]bool{
		"gpt-6": true, "OpenAI/GPT-6": true, "gpt-6 ": true,
		"gpt-6-luna": false, "llama3": false, "": false,
	} {
		got := rootAuthorityOf(&run, FamilyLockBlocking, benchNow, maxAge, running)
		if got.Granted != want {
			t.Errorf("running %q: granted %v, want %v (%s)", running, got.Granted, want,
				got.Reason)
		}
		if got.MeasuredModel != "gpt-6" || got.RunningModel != modellift.NormalizeModel(running) {
			t.Errorf("running %q: measured %q running %q", running, got.MeasuredModel,
				got.RunningModel)
		}
	}
	got := rootAuthorityOf(&run, FamilyLockBlocking, benchNow, maxAge, "gpt-6-luna")
	if !strings.Contains(got.Reason, `measured with "gpt-6"; the investigator runs `+
		`"gpt-6-luna"`) {
		t.Fatalf("reason %q, want the mismatch named", got.Reason)
	}
	if got = rootAuthorityOf(&run, FamilyLockBlocking, benchNow, maxAge, ""); !strings.
		Contains(got.Reason, "the investigator's model is unknown") {
		t.Fatalf("reason %q, want the unknown running model named", got.Reason)
	}
}

func TestRootAuthority_NoMeasurementStillNamesTheRunningModel(t *testing.T) {
	got := rootAuthorityOf(nil, FamilyLockBlocking, benchNow,
		DefaultThresholds().BenchMaxAge, "gpt-6")
	if got.Granted || got.RunningModel != "gpt-6" || got.MeasuredModel != "" ||
		!strings.Contains(got.Reason, "no held-out") {
		t.Fatalf("authority = %+v", got)
	}
}

func TestModelLift_StoresTheModelUnderItsJSONBKey(t *testing.T) {
	raw, err := json.Marshal(ModelLift{Mode: "live", Model: "gpt-6", Records: []LiftRecord{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"llm_model":"gpt-6"`) {
		t.Fatalf("model lift JSON %s, want the llm_model key the ledger query reads", raw)
	}
	var back ModelLift
	if err := json.Unmarshal([]byte(`{"llm_mode": "live", "records": []}`), &back); err != nil ||
		back.Model != "" {
		t.Fatalf("a stored lift from before llm-01 reads with no model: %+v (%v)", back, err)
	}
}
