package sre

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentloop"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sre-03: a read the investigator commits is citable with its typed
// values, scrubbed exactly like the text the model sees.

func TestResultForModel_CarriesScrubbedTypedValues(t *testing.T) {
	res := probes.Result{ProbeID: probes.PlanRegressions, Version: "v1",
		Status: probes.StatusOK, Columns: []string{"queryid", "mean_ms", "app"},
		Rows: []probes.Row{
			{"queryid": json.Number("-8123456789012345678"), "mean_ms": 75.3125,
				"app": "failed: password=hunter2 host=db"},
			{"queryid": int64(42), "mean_ms": 1.5, "app": "worker"}}}
	text, values := resultForModel(res)
	if !strings.Contains(text, "queryid=-8123456789012345678") ||
		strings.Contains(text, "hunter2") {
		t.Fatalf("text = %q", text)
	}
	got := strings.Join(values, "|")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("values leak the secret: %q", got)
	}
	for _, want := range []string{"-8123456789012345678", "75.31", "42", "1.5", "worker"} {
		if !containsValue(values, want) {
			t.Errorf("values %q lack %q", got, want)
		}
	}
	if containsValue(values, "2") || containsValue(values, "v1") {
		t.Errorf("values %q carry a format artifact", got)
	}
}

func TestResultForModel_ValuesStopAtTheShownRows(t *testing.T) {
	res := probes.Result{ProbeID: probes.LongTransactions, Version: "v1",
		Status: probes.StatusOK, Columns: []string{"pid"}}
	for i := 0; i < maxResultRows+3; i++ {
		res.Rows = append(res.Rows, probes.Row{"pid": int64(9000 + i)})
	}
	_, values := resultForModel(res)
	if len(values) != maxResultRows || containsValue(values, "9020") {
		t.Fatalf("%d values (%q), want the %d shown rows", len(values), values,
			maxResultRows)
	}
	cited := []agentloop.Evidence{{ID: "r", Values: values}}
	if err := groundClaim("pid 9020 is open.", cited); err == nil {
		t.Fatal("a row the model never saw grounded a claim")
	}
}

func containsValue(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
