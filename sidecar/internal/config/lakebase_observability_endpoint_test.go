package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	lbHost     = "ep-test.database.example.com"
	lbPrimary  = "projects/search/branches/test/endpoints/primary"
	lbExplicit = "projects/search/branches/test/endpoints/explicit"
)

// lakebaseYAML is a disabled history config whose one binding may omit
// endpoint; bindings are validated whether or not reads are enabled.
func lakebaseYAML(endpoint string) string {
	y := "lakebase_observability:\n  bindings:\n    - host: " + lbHost +
		"\n      database: sage_test\n"
	if endpoint != "" {
		y += "      endpoint: " + endpoint + "\n"
	}
	return y
}

func clearLakebaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", "")
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", "")
}

func loadLakebase(t *testing.T, endpoint string) (*Config, error) {
	t.Helper()
	chdirTemp(t)
	return Load([]string{"--config", writeYAMLFile(t, lakebaseYAML(endpoint))})
}

func TestLakebaseBindingInheritsFleetEndpointMapping(t *testing.T) {
	clearLakebaseEnv(t)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS",
		`{"other.example.com": "projects/x/branches/y/endpoints/z", `+
			`"EP-TEST.database.example.com.": "`+lbPrimary+`"}`)
	cfg, err := loadLakebase(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LakebaseObservability.Bindings[0].Endpoint; got != lbPrimary {
		t.Fatalf("inherited endpoint = %q, want the mapping for the host %q", got, lbPrimary)
	}
}

func TestLakebaseBindingInheritsSingleEndpoint(t *testing.T) {
	clearLakebaseEnv(t)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", lbPrimary)
	cfg, err := loadLakebase(t, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LakebaseObservability.Bindings[0].Endpoint; got != lbPrimary {
		t.Fatalf("inherited endpoint = %q, want %q", got, lbPrimary)
	}
}

func TestLakebaseBindingWithoutEndpointOrMappingFails(t *testing.T) {
	clearLakebaseEnv(t)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"other.example.com": "`+lbPrimary+`"}`)
	_, err := loadLakebase(t, "")
	want := "binding for " + lbHost + " has no endpoint and SAGE_LAKEBASE_ENDPOINT(S) maps none"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Load error = %v, want one containing %q", err, want)
	}
}

func TestLakebaseBindingNoEnvironmentAtAllFails(t *testing.T) {
	clearLakebaseEnv(t)
	_, err := loadLakebase(t, "")
	if err == nil || !strings.Contains(err.Error(), "has no endpoint") {
		t.Fatalf("Load error = %v, want the missing-endpoint refusal", err)
	}
}

func TestLakebaseBindingKeepsExplicitEndpoint(t *testing.T) {
	clearLakebaseEnv(t)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", lbPrimary)
	cfg, err := loadLakebase(t, lbExplicit)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.LakebaseObservability.Bindings[0].Endpoint; got != lbExplicit {
		t.Fatalf("explicit endpoint replaced by %q", got)
	}
}

func TestLakebaseBindingInheritKeepsBothVariablesError(t *testing.T) {
	clearLakebaseEnv(t)
	t.Setenv("SAGE_LAKEBASE_ENDPOINT", lbPrimary)
	t.Setenv("SAGE_LAKEBASE_ENDPOINTS", `{"`+lbHost+`": "`+lbPrimary+`"}`)
	_, err := loadLakebase(t, "")
	want := "set only one of SAGE_LAKEBASE_ENDPOINT and SAGE_LAKEBASE_ENDPOINTS"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Load error = %v, want one containing %q", err, want)
	}
}

func TestLakebaseBindingAcceptsLeadingDigitEndpoint(t *testing.T) {
	clearLakebaseEnv(t)
	cfg, err := loadLakebase(t, "projects/1abc/branches/br-1/endpoints/ep-1")
	if err != nil {
		t.Fatalf("an endpoint OAuth accepts was refused by telemetry config: %v", err)
	}
	if got := cfg.LakebaseObservability.Bindings[0].Endpoint; got !=
		"projects/1abc/branches/br-1/endpoints/ep-1" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestLakebaseBindingVerdictsMatchSharedTable(t *testing.T) {
	raw, err := os.ReadFile("../lakebaseid/testdata/resources.json")
	if err != nil {
		t.Fatalf("read shared verdict table: %v", err)
	}
	var cases []struct {
		Name, Resource string
		Valid          bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("parse shared verdict table: %v (%d cases)", err, len(cases))
	}
	for _, c := range cases {
		cfg := LakebaseObservabilityConfig{Bindings: []LakebaseObservabilityBinding{
			{Host: lbHost, Database: "sage_test", Endpoint: c.Resource}}}
		if err := cfg.validate(); (err == nil) != c.Valid {
			t.Errorf("%s: endpoint %q validate = %v, want valid=%v",
				c.Name, c.Resource, err, c.Valid)
		}
	}
}

func TestLakebaseWindowSecondsDocDescribesReadWidth(t *testing.T) {
	f, ok := reflect.TypeOf(LakebaseObservabilityConfig{}).FieldByName("WindowSeconds")
	if !ok {
		t.Fatal("WindowSeconds field missing")
	}
	doc := f.Tag.Get("doc")
	if strings.Contains(doc, "Initial reconciliation") {
		t.Fatalf("window_seconds doc still calls it an initial window: %q", doc)
	}
	if !strings.Contains(doc, "width of each forward and reconcile read") {
		t.Fatalf("window_seconds doc %q does not say it bounds every read", doc)
	}
}

func TestLakebaseBindingsEndpointDocSaysOptional(t *testing.T) {
	f, _ := reflect.TypeOf(LakebaseObservabilityConfig{}).FieldByName("Bindings")
	if doc := f.Tag.Get("doc"); !strings.Contains(doc, "SAGE_LAKEBASE_ENDPOINT") {
		t.Fatalf("bindings doc %q does not say where an omitted endpoint comes from", doc)
	}
}

// No concurrent access tests: config is loaded once per process before any
// goroutine reads it; inheritance runs inside that single Load call.
