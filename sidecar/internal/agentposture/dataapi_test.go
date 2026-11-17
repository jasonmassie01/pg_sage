package agentposture

import (
	"strings"
	"testing"
)

// fleetpol-02c: AP-03 and AP-04 findings on Supabase carry whether the
// project's Data API serves the finding's schema. Known not exposed lowers
// a critical finding to warning; unknown stays critical (never inferred
// good). Off Supabase the field is absent.
//
// No concurrent access tests here: these functions are pure over values.

func supabasePlatform(schemas ...string) Platform {
	p := Platform{Provider: "supabase"}
	if schemas != nil {
		p.DataAPI = &DataAPIExposure{Schemas: schemas}
	}
	return p
}

func criticalTable(schema string) Finding {
	return Finding{Detector: "AP-03", Severity: Critical, ObjectType: "table",
		Object: schema + ".leads", Detail: "leads is granted SELECT to anon."}
}

func TestDataAPIState(t *testing.T) {
	cases := []struct {
		name   string
		p      Platform
		schema string
		want   string
	}{
		{"self-managed", Platform{Provider: "self-managed"}, "public", ""},
		{"unknown provider", Platform{}, "public", ""},
		{"rds with a stray exposure", Platform{Provider: "rds",
			DataAPI: &DataAPIExposure{Schemas: []string{"api"}}}, "public", ""},
		{"supabase not read", supabasePlatform(), "public", DataAPIUnknown},
		{"supabase empty list", Platform{Provider: "supabase",
			DataAPI: &DataAPIExposure{}}, "public", DataAPIUnknown},
		{"supabase exposes public", supabasePlatform("public", "graphql_public"), "public",
			DataAPIExposed},
		{"supabase exposes only api", supabasePlatform("api"), "public", DataAPINotExposed},
		{"names are case-sensitive", supabasePlatform("Public"), "public",
			DataAPINotExposed},
		{"empty schema is unknown", supabasePlatform("api"), "", DataAPIUnknown},
	}
	for _, c := range cases {
		if got := c.p.dataAPIState(c.schema); got != c.want {
			t.Errorf("%s: state = %q, want %q", c.name, got, c.want)
		}
	}
}

// Happy path: an exposure of public keeps the finding critical; one of
// only api lowers a public finding to warning.
func TestWithDataAPI_SeverityFollowsTheExposure(t *testing.T) {
	got := withDataAPI(supabasePlatform("public"), "public", criticalTable("public"))
	if got.Severity != Critical || got.DataAPIExposed != DataAPIExposed {
		t.Fatalf("public exposed: %s/%q, want critical/true", got.Severity, got.DataAPIExposed)
	}
	requireContains(t, "exposed detail", got.Detail, "Data API", "public")
	got = withDataAPI(supabasePlatform("api"), "public", criticalTable("public"))
	if got.Severity != Warning || got.DataAPIExposed != DataAPINotExposed {
		t.Fatalf("only api exposed: %s/%q, want warning/false", got.Severity,
			got.DataAPIExposed)
	}
	requireContains(t, "not exposed detail", got.Detail, "does not serve schema public")
	if !strings.HasPrefix(got.Detail, "leads is granted SELECT to anon.") {
		t.Fatalf("detail %q lost the detector's own text", got.Detail)
	}
}

func TestWithDataAPI_UnknownStaysCritical(t *testing.T) {
	got := withDataAPI(supabasePlatform(), "public", criticalTable("public"))
	if got.Severity != Critical || got.DataAPIExposed != DataAPIUnknown {
		t.Fatalf("unknown: %s/%q, want critical/unknown", got.Severity, got.DataAPIExposed)
	}
	requireContains(t, "unknown detail", got.Detail, "SAGE_SUPABASE_OBSERVABILITY_TOKEN")
}

// Off Supabase nothing changes: no field, no text, same severity.
func TestWithDataAPI_OffSupabaseUnchanged(t *testing.T) {
	in := criticalTable("public")
	got := withDataAPI(Platform{Provider: "rds"}, "public", in)
	if got.Severity != Critical || got.DataAPIExposed != "" || got.Detail != in.Detail {
		t.Fatalf("off Supabase the finding changed: %+v", got)
	}
	if _, ok := got.AnalyzerFinding().Detail["data_api_exposed"]; ok {
		t.Fatal("off Supabase the analyzer detail carries data_api_exposed")
	}
}

// Boundary: only critical is lowered; a warning (AP-04) or info stays.
func TestWithDataAPI_LowersOnlyCritical(t *testing.T) {
	for _, sev := range []Severity{Warning, Info} {
		in := criticalTable("public")
		in.Severity = sev
		got := withDataAPI(supabasePlatform("api"), "public", in)
		if got.Severity != sev || got.DataAPIExposed != DataAPINotExposed {
			t.Fatalf("%s: got %s/%q", sev, got.Severity, got.DataAPIExposed)
		}
	}
}

func TestAnalyzerFinding_CarriesDataAPIExposed(t *testing.T) {
	for _, state := range []string{DataAPIExposed, DataAPINotExposed, DataAPIUnknown} {
		f := criticalTable("public")
		f.DataAPIExposed = state
		if got := f.AnalyzerFinding().Detail["data_api_exposed"]; got != state {
			t.Fatalf("detail data_api_exposed = %v, want %q", got, state)
		}
	}
}

// The posture fingerprint folds the Data API state in, so a change of the
// state alone makes the next sweep run; the order of the list does not
// matter and off Supabase nothing is folded in.
func TestDataAPIFingerprint(t *testing.T) {
	if got := (Platform{Provider: "rds"}).dataAPIFingerprint(); got != 0 {
		t.Fatalf("off Supabase fingerprint = %d, want 0", got)
	}
	unknown := supabasePlatform().dataAPIFingerprint()
	public := supabasePlatform("public", "api").dataAPIFingerprint()
	api := supabasePlatform("api").dataAPIFingerprint()
	if unknown == 0 || public == 0 || api == 0 {
		t.Fatalf("a Supabase state folded in nothing: %d %d %d", unknown, public, api)
	}
	if unknown == public || public == api || unknown == api {
		t.Fatalf("states collide: unknown %d, public+api %d, api %d", unknown, public, api)
	}
	if got := supabasePlatform("api", "public").dataAPIFingerprint(); got != public {
		t.Fatalf("order changed the fingerprint: %d vs %d", got, public)
	}
	if got := supabasePlatform("a,b").dataAPIFingerprint(); got ==
		supabasePlatform("a", "b").dataAPIFingerprint() {
		t.Fatal("a schema named a,b collides with schemas a and b")
	}
}
