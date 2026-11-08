package causal

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

func TestBoundApplicationSLOClaimsImpactButRejectsExternalAndProxyRows(t *testing.T) {
	for _, source := range []string{"push", "prometheus", ""} {
		row := sloRow("bound-checkout", "app", "page", 16, true, "")
		row["source"] = source
		d := WithSLO(Diagnosis{}, []Observation{obs("S1", probes.ApplicationSLOStatus, row)})
		if d.Impact == nil {
			t.Fatal("scoped SLO evidence ignored")
		}
		if source == "push" && d.Impact.State != ImpactBurning {
			t.Fatalf("bound pushed app SLI lost: %+v", d.Impact)
		}
		if source != "push" && d.Impact.State != ImpactUnknown {
			t.Fatalf("external SLO relabeled database-scoped: %+v", d.Impact)
		}
	}
	row := sloRow("proxy", "proxy", "page", 16, true, "")
	row["source"] = "push"
	d := WithSLO(Diagnosis{}, []Observation{obs("S1", probes.ApplicationSLOStatus, row)})
	if d.Impact == nil || d.Impact.State != ImpactUnknown {
		t.Fatalf("proxy claimed scoped app impact: %+v", d.Impact)
	}
}
