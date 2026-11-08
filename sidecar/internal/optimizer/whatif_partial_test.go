package optimizer

import (
	"errors"
	"strings"
	"testing"
)

// The partial-measurement reason (indexes-06). No concurrent-access test:
// whatIfVerdict is a pure function of its arguments.

func TestWhatIfVerdict_PartialRegressionIsUnverifiedNotRejected(t *testing.T) {
	verdict, reason := whatIfVerdict(WhatIfResult{Measured: 2, Failed: 1,
		Improvement: -12.5, SizeBytes: 8192, UnplannedQueryIDs: []int64{7}}, nil, 10)
	want := "planned statements regressed 12.5% (call-weighted, 2 of 3); " +
		"not planned: queryid 7"
	if verdict != WhatIfUnverified || reason != want {
		t.Fatalf("verdict %q reason %q, want unverified %q", verdict, reason, want)
	}
}

func TestWhatIfVerdict_PartialListsAtMostTenQueryIDs(t *testing.T) {
	ids := make([]int64, 12)
	for i := range ids {
		ids[i] = int64(100 + i)
	}
	_, reason := whatIfVerdict(WhatIfResult{Measured: 3, Failed: 12, Improvement: 40,
		SizeBytes: 1, UnplannedQueryIDs: ids}, nil, 10)
	want := "not planned: queryid 100, 101, 102, 103, 104, 105, 106, 107, 108, 109 " +
		"and 2 more"
	if !strings.HasSuffix(reason, want) || !strings.Contains(reason, "3 of 15") {
		t.Fatalf("reason = %q, want suffix %q", reason, want)
	}
}

func TestWhatIfVerdict_PartialWithoutQueryIDsCountsThem(t *testing.T) {
	_, reason := whatIfVerdict(WhatIfResult{Measured: 1, Failed: 2, Improvement: 5,
		SizeBytes: 1}, nil, 10)
	want := "planned statements improved 5.0% (call-weighted, 1 of 3); " +
		"not planned: 2 of 3 statements"
	if reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

func TestWhatIfVerdict_CompleteMeasurementUnchanged(t *testing.T) {
	if v, r := whatIfVerdict(WhatIfResult{Measured: 2, Improvement: 50, SizeBytes: 1},
		nil, 10); v != WhatIfVerified || r != "" {
		t.Fatalf("complete improvement: %q %q", v, r)
	}
	if v, _ := whatIfVerdict(WhatIfResult{Measured: 2, Improvement: 5, SizeBytes: 1},
		nil, 10); v != WhatIfRejected {
		t.Fatalf("complete shortfall must still reject: %q", v)
	}
	if v, r := whatIfVerdict(WhatIfResult{}, errors.New("boom"), 10); v != WhatIfUnverified ||
		r != "what-if evaluation failed: boom" {
		t.Fatalf("error: %q %q", v, r)
	}
}

func TestHypoPGCannotModel(t *testing.T) {
	cases := map[string]string{
		"CREATE INDEX i ON public.t (a)":            "",
		"CREATE INDEX i ON public.t USING BRIN (a)": "",
		"not sql": "",
		"CREATE INDEX i ON public.t USING gin (a)": "HypoPG cannot model gin indexes; " +
			"operator approval required",
	}
	for ddl, want := range cases {
		if got := HypoPGCannotModel(ddl); got != want {
			t.Fatalf("%q: %q, want %q", ddl, got, want)
		}
	}
}
