package auditjob

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// No concurrent access test: regressedFinding is a pure function.

// The regression finding names the chain, its head and each stopped sink,
// and warns that a re-anchor past the first differing position loses the
// links before it: the re-anchor cannot prove which ones the SIEM holds.
func TestRegressedFindingContent(t *testing.T) {
	spec, ok := auditchain.SpecFor("action_log")
	if !ok {
		t.Fatal("no action_log spec")
	}
	sinks := []regressedSink{{Sink: "soc", Seq: 10, Reason: "link 10 is missing"}}
	f := regressedFinding("orders", spec, 7, sinks)
	if f.Category != RegressedCategory || f.Severity != "critical" ||
		f.ObjectType != "audit_trail" || f.ObjectIdentifier != ObjectIdentifier(spec) ||
		f.DatabaseName != "orders" {
		t.Fatalf("finding = %+v", f)
	}
	if f.Detail["chain"] != "action_log" || f.Detail["head_seq"] != int64(7) {
		t.Fatalf("detail = %+v", f.Detail)
	}
	got, ok := f.Detail["sinks"].([]regressedSink)
	if !ok || len(got) != 1 || got[0] != sinks[0] {
		t.Fatalf("detail sinks = %+v", f.Detail["sinks"])
	}
	for _, want := range []string{"/api/v1/audit/siem/{sink}/reanchor",
		"from_seq 1 is always safe", "Links before from_seq are not re-sent"} {
		if !strings.Contains(f.Recommendation, want) {
			t.Fatalf("recommendation %q does not say %q", f.Recommendation, want)
		}
	}
}
