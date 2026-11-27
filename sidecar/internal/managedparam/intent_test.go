package managedparam

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestIntentDetailRoundTrip(t *testing.T) {
	in, err := NewIntent("rds", "shared_buffers", "4GB", "requires a restart")
	if err != nil {
		t.Fatal(err)
	}
	// The detail survives the findings table: JSON in, JSON out.
	raw, err := json.Marshal(map[string]any{DetailKey: in.Detail()})
	if err != nil {
		t.Fatal(err)
	}
	var detail map[string]any
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatal(err)
	}
	out, ok := IntentFromDetail(detail)
	if !ok || out != in {
		t.Fatalf("round trip = %+v, %t; want %+v", out, ok, in)
	}
}

func TestIntentFromDetailRejectsMalformed(t *testing.T) {
	cases := []map[string]any{
		nil,
		{},
		{DetailKey: "not an object"},
		{DetailKey: map[string]any{"provider": "rds", "parameter": "shared_buffers"}},
		{DetailKey: map[string]any{"provider": "neon", "parameter": "work_mem", "value": "4MB"}},
		{DetailKey: map[string]any{"provider": "rds", "parameter": "Bad Name", "value": "1"}},
		{DetailKey: map[string]any{"provider": "rds", "parameter": "work_mem",
			"value": "1; DROP TABLE x"}},
		{DetailKey: map[string]any{"provider": 7, "parameter": "work_mem", "value": "1"}},
	}
	for i, detail := range cases {
		if got, ok := IntentFromDetail(detail); ok {
			t.Errorf("case %d: IntentFromDetail = %+v, want rejection", i, got)
		}
	}
}

func TestNewIntentValidates(t *testing.T) {
	cases := []struct {
		provider, parameter, value string
		want                       error
	}{
		{"azure-cosmos", "work_mem", "4MB", ErrUnsupportedProvider},
		{"azure-single", "work_mem", "4MB", ErrUnsupportedProvider},
		{"azure", "work_mem", "4MB; reboot", ErrInvalidValue},
		{"", "work_mem", "4MB", ErrUnsupportedProvider},
		{"rds", "", "4MB", ErrInvalidParameter},
		{"rds", "work_mem;", "4MB", ErrInvalidParameter},
		{"rds", "work_mem", "", ErrInvalidValue},
		{"rds", "work_mem", "4MB --apply", ErrInvalidValue},
		{"rds", "work_mem", "$(reboot)", ErrInvalidValue},
		{"cloud-sql", "work_mem", "`id`", ErrInvalidValue},
	}
	for _, tc := range cases {
		if _, err := NewIntent(tc.provider, tc.parameter, tc.value, "r"); !errors.Is(err, tc.want) {
			t.Errorf("NewIntent(%q,%q,%q) err = %v, want %v", tc.provider, tc.parameter,
				tc.value, err, tc.want)
		}
	}
}

// advisors-03: AlloyDB and Azure flexible server became supported (the
// rows pinned "" while their findings stayed guidance text); Cosmos DB for
// PostgreSQL and the retired single server stay unsupported.
func TestNormalizeProvider(t *testing.T) {
	cases := map[string]string{"rds": "rds", "RDS": "rds", "aws": "rds",
		"aurora": "aurora", "cloud-sql": "cloud-sql", "cloudsql": "cloud-sql",
		"gcp": "cloud-sql", "alloydb": "alloydb", "AlloyDB": "alloydb", "azure": "azure",
		"azure-flexible": "azure", "azure-cosmos": "", "azure-single": "",
		" Azure-Single ": "", "neon": "", "": ""}
	for in, want := range cases {
		if got := NormalizeProvider(in); got != want {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", in, got, want)
		}
		if Supported(in) != (want != "") {
			t.Errorf("Supported(%q) = %t", in, Supported(in))
		}
	}
}

// No concurrent access tests: intentRefusal is a pure function.
func TestIntentRefusal(t *testing.T) {
	ok := func(param string) Intent { return Intent{Provider: "rds", Parameter: param} }
	cases := []struct {
		category, parameter string
		refused             bool
	}{
		{"memory_tuning", "shared_buffers", false},
		{"vacuum_tuning", "autovacuum_naptime", false},
		{"wal_tuning", "max_wal_size", false},
		{"connection_tuning", "max_connections", false},
		// Executable only per database (advisors-07): never a provider-wide change.
		{"connection_tuning", "idle_in_transaction_session_timeout", true},
		{"query_rewrite", "work_mem", true},
		{"bloat_remediation", "work_mem", true},
		{"", "work_mem", true},
		{"Memory_Tuning", "work_mem", true},
		{"memory_tuning", "rds.force_ssl", true},
		{"memory_tuning", "wal_level", true},
		{"memory_tuning", "", true},
	}
	for _, tc := range cases {
		why := intentRefusal(tc.category, ok(tc.parameter))
		if (why != "") != tc.refused {
			t.Errorf("%s/%s: refusal %q, want refused=%t", tc.category, tc.parameter,
				why, tc.refused)
		}
		if tc.refused && !strings.Contains(why, tc.category) &&
			!strings.Contains(why, tc.parameter) {
			t.Errorf("%s/%s: refusal %q names neither", tc.category, tc.parameter, why)
		}
	}
}

func TestProducerCategoriesIsACopy(t *testing.T) {
	a := ProducerCategories()
	if len(a) != 4 {
		t.Fatalf("producers = %v, want the four config categories", a)
	}
	a[0] = "query_rewrite"
	if intentRefusal("query_rewrite", Intent{Parameter: "work_mem"}) == "" {
		t.Fatal("mutating the returned slice changed the producer list")
	}
}
