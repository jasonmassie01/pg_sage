package managedparam

import (
	"encoding/json"
	"errors"
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
		{"azure", "work_mem", "4MB", ErrUnsupportedProvider},
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

func TestNormalizeProvider(t *testing.T) {
	cases := map[string]string{"rds": "rds", "RDS": "rds", "aws": "rds",
		"aurora": "aurora", "cloud-sql": "cloud-sql", "cloudsql": "cloud-sql",
		"gcp": "cloud-sql", "alloydb": "", "azure": "", "neon": "", "": ""}
	for in, want := range cases {
		if got := NormalizeProvider(in); got != want {
			t.Errorf("NormalizeProvider(%q) = %q, want %q", in, got, want)
		}
		if Supported(in) != (want != "") {
			t.Errorf("Supported(%q) = %t", in, Supported(in))
		}
	}
}
