package main

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// sre-03 Phase 2 wiring: sre.investigator.require_support_citation
// reaches every database's investigator, on by default; review mode has
// no investigator to carry it.

func TestSREInvestigatorConfig_CarriesTheSupportCitationRule(t *testing.T) {
	settings := config.DefaultConfig().SRE
	got := sreInvestigatorConfig(settings, nil, nil)
	if got == nil || !got.RequireSupportCitation {
		t.Fatalf("default settings = %+v, want the support-citation rule on", got)
	}
	settings.Investigator.RequireSupportCitation = false
	got = sreInvestigatorConfig(settings, nil, nil)
	if got == nil || got.RequireSupportCitation {
		t.Fatalf("require_support_citation false = %+v, want the rule off", got)
	}
	settings.Investigator.RequireSupportCitation = true
	settings.LLM.Mode = config.SRELLMModeReview
	if got := sreInvestigatorConfig(settings, nil, nil); got != nil {
		t.Fatalf("review mode = %+v, want no investigator", got)
	}
}
