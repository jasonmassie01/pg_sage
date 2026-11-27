package firstlook

import (
	"strings"
	"testing"
)

// rca-04: pg_sage ingests a self-managed server's executed auto_explain
// plans from its csvlog/jsonlog file only when they are JSON and carry a
// query identifier, so the enablement hint sets log_format, log_verbose
// (the identifier is printed only in verbose output) and compute_query_id.
func TestAutoExplainHintAsksForIngestiblePlans(t *testing.T) {
	e := allReady()
	e.AutoExplainLoaded = false
	caps, _ := ExtensionCapabilities(e, "self-managed")
	a := capability(caps, CapAutoExplain)
	if a.Status != CapabilityNotLoaded || len(a.Steps) != 3 {
		t.Fatalf("auto_explain = %+v, want three not_loaded steps", a)
	}
	for _, want := range []string{
		"ALTER SYSTEM SET auto_explain.log_min_duration = '1s';",
		"ALTER SYSTEM SET auto_explain.log_format = 'json';",
		"ALTER SYSTEM SET auto_explain.log_verbose = on;",
		"ALTER SYSTEM SET compute_query_id = on;",
	} {
		if !strings.Contains(a.Steps[1], want) {
			t.Fatalf("settings step %q lacks %q", a.Steps[1], want)
		}
	}
	if !strings.Contains(a.Steps[2], "pg_reload_conf") {
		t.Fatalf("the reload must stay the last step: %q", a.Steps[2])
	}
}

// Loaded auto_explain needs no steps; managed providers keep their own.
func TestAutoExplainHintOnlyForSelfManaged(t *testing.T) {
	e := allReady()
	caps, _ := ExtensionCapabilities(e, "self-managed")
	if a := capability(caps, CapAutoExplain); a.Status != CapabilityOK || len(a.Steps) != 0 {
		t.Fatalf("loaded auto_explain = %+v, want ok with no steps", a)
	}
	e.AutoExplainLoaded = false
	caps, _ = ExtensionCapabilities(e, "rds")
	for _, step := range capability(caps, CapAutoExplain).Steps {
		if strings.Contains(step, "ALTER SYSTEM") {
			t.Fatalf("rds step %q uses ALTER SYSTEM", step)
		}
	}
}
