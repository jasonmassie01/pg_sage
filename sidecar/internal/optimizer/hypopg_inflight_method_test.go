package optimizer

import (
	"context"
	"testing"
)

// An in-flight index HypoPG cannot model (indexes-06): the candidate would
// be judged against an incomplete baseline, so it stays unverified with the
// in-flight method named, and no hypothetical create is attempted (which
// would fail every what-if on the table as "what-if evaluation failed").

func TestEnrich_InFlightGINKeepsTheCandidateUnverified(t *testing.T) {
	o, w, logs := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2,
		Improvement: 50, SizeBytes: 8192})
	rec := candidate(statusDDL)
	rec.Alongside = []string{"CREATE INDEX CONCURRENTLY IF NOT EXISTS pending_gin ON " +
		"public.ai_claims USING GIN (payload)"}
	got, rejected := o.enrichWithHypoPG(context.Background(), rec, admitTable())
	want := "HypoPG cannot model the in-flight gin index pending_gin on this table; " +
		"operator approval required"
	if rejected || got.WhatIf != WhatIfUnverified || got.Validated || got.WhatIfReason != want {
		t.Fatalf("rejected=%t verdict=%q reason=%q, want unverified %q", rejected,
			got.WhatIf, got.WhatIfReason, want)
	}
	if w.calls.Load() != 0 || len(logs.matching("hypopg validation failed")) != 0 {
		t.Fatalf("what-ifs %d, logs %q: nothing is created for an unmodellable baseline",
			w.calls.Load(), logs.lines)
	}
}

func TestEnrich_InFlightUnnamedHNSWNamesTheMethod(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2,
		Improvement: 50, SizeBytes: 8192})
	rec := candidate(statusDDL)
	rec.Alongside = []string{"CREATE INDEX ON public.ai_claims USING hnsw (embedding)"}
	got, _ := o.enrichWithHypoPG(context.Background(), rec, admitTable())
	want := "HypoPG cannot model the in-flight hnsw index on this table; " +
		"operator approval required"
	if got.WhatIf != WhatIfUnverified || got.WhatIfReason != want {
		t.Fatalf("verdict=%q reason=%q, want %q", got.WhatIf, got.WhatIfReason, want)
	}
}

func TestEnrich_InFlightModellableIndexesReachTheWhatIf(t *testing.T) {
	for _, alongside := range [][]string{nil, {}, {"not sql"},
		{"CREATE INDEX a ON public.ai_claims USING brin (created_at)",
			"CREATE INDEX b ON public.ai_claims (status, id)"}} {
		o, w, _ := admissionOptimizer(newMemStore(), WhatIfResult{Measured: 2,
			Improvement: 50, SizeBytes: 8192})
		rec := candidate(statusDDL)
		rec.Alongside = alongside
		got, _ := o.enrichWithHypoPG(context.Background(), rec, admitTable())
		if w.calls.Load() != 1 || got.WhatIf != WhatIfVerified {
			t.Fatalf("alongside %q: what-ifs %d, verdict %q (%s)", alongside,
				w.calls.Load(), got.WhatIf, got.WhatIfReason)
		}
	}
}

// The candidate's own method is reported before an in-flight one's.
func TestEnrich_CandidateMethodOutranksInFlightMethod(t *testing.T) {
	o, _, _ := admissionOptimizer(newMemStore(), WhatIfResult{})
	rec := candidate(ginDDL)
	rec.Alongside = []string{"CREATE INDEX g ON public.ai_claims USING gist (status)"}
	got, _ := o.enrichWithHypoPG(context.Background(), rec, ginTable())
	if got.WhatIfReason != "HypoPG cannot model gin indexes; operator approval required" {
		t.Fatalf("reason = %q", got.WhatIfReason)
	}
}
