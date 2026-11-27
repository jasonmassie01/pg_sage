package inbox

import (
	"sync"
	"testing"
	"time"
)

func resetMetrics(t *testing.T) {
	t.Helper()
	metrics.mu.Lock()
	metrics.items, metrics.decisions = nil, map[seriesKey]*DecisionSeries{}
	metrics.mu.Unlock()
}

func TestSurfaceKeepsLabelsBounded(t *testing.T) {
	for in, want := range map[string]string{"inbox": "inbox", " Chat ": "chat",
		"page": "page", "cli": "cli", "api": "api", "": "api", "evil\"label": "api",
		"INBOX": "inbox"} {
		if got := Surface(in); got != want {
			t.Errorf("Surface(%q) = %q, want %q", in, got, want)
		}
	}
}

func seriesOf(kind Kind, surface string) *DecisionSeries {
	_, all := Snapshot()
	for _, s := range all {
		if s.Kind == kind && s.Surface == surface {
			return &s
		}
	}
	return nil
}

func TestRecordDecisionBucketsAtTheirBounds(t *testing.T) {
	resetMetrics(t)
	RecordDecision(KindFact, "inbox", time.Minute)              // exactly 60: first bucket
	RecordDecision(KindFact, "inbox", time.Minute+time.Second)  // second bucket
	RecordDecision(KindFact, "inbox", 8*24*time.Hour)           // over a week: +Inf only
	RecordDecision(KindFact, "inbox", -time.Hour)               // clock skew: zero
	RecordDecision(KindChangeApproval, "nonsense", 2*time.Hour) // counted as api
	s := seriesOf(KindFact, "inbox")
	if s == nil || s.Count != 4 || s.Buckets[0] != 2 || s.Buckets[1] != 1 {
		t.Fatalf("fact/inbox = %+v, want 4 with 2 in <=60s and 1 in <=300s", s)
	}
	var inBuckets uint64
	for _, n := range s.Buckets {
		inBuckets += n
	}
	if inBuckets != 3 || s.Sum != 60+61+8*24*3600 {
		t.Fatalf("buckets hold %d (want 3, one over a week) sum %v", inBuckets, s.Sum)
	}
	if api := seriesOf(KindChangeApproval, "api"); api == nil || api.Count != 1 ||
		api.Buckets[4] != 1 {
		t.Fatalf("change/api = %+v, want one in the 4h bucket", api)
	}
	if seriesOf(KindChangeApproval, "nonsense") != nil {
		t.Fatal("an unknown surface minted a label")
	}
}

func TestObserveItemsNeverRecordsALowerBound(t *testing.T) {
	resetMetrics(t)
	if items, _ := Snapshot(); items != nil {
		t.Fatalf("items before any read = %v, want nil", items)
	}
	full := Result{CountsByKind: map[Kind]int{}}
	for _, k := range Kinds() {
		full.CountsByKind[k] = 1
	}
	full.CountsByKind[KindFact] = 3
	ObserveItems(full)
	partial := Result{CountsByKind: map[Kind]int{KindFact: 9}}
	ObserveItems(partial)
	lower := full
	lower.LowerBound = true
	lower.CountsByKind = map[Kind]int{}
	for _, k := range Kinds() {
		lower.CountsByKind[k] = 0
	}
	ObserveItems(lower)
	items, _ := Snapshot()
	if len(items) != 8 || items[KindFact] != 3 || items[KindPlanApproval] != 1 {
		t.Fatalf("items = %v, want the complete read only", items)
	}
	items[KindFact] = 100
	if again, _ := Snapshot(); again[KindFact] != 3 {
		t.Fatal("Snapshot shares its map with the recorder")
	}
}

func TestRecordDecisionConcurrently(t *testing.T) {
	resetMetrics(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordDecision(KindAgentRequest, "chat", time.Second)
			_, _ = Snapshot()
		}()
	}
	wg.Wait()
	if s := seriesOf(KindAgentRequest, "chat"); s == nil || s.Count != 50 ||
		s.Buckets[0] != 50 {
		t.Fatalf("concurrent records = %+v, want 50", s)
	}
}

// Boundary: /metrics exports at most eight buckets per histogram, +Inf
// included (cmd/pg_sage_sidecar TestContractGaugeAndCounterSuffixes, which
// sees this family only once a decision was recorded), and the bounds rise.
func TestDecisionBoundsFitTheMetricsContract(t *testing.T) {
	if n := len(DecisionBounds) + 1; n < 2 || n > 8 {
		t.Fatalf("%d buckets with +Inf, want 2-8", n)
	}
	for i := 1; i < len(DecisionBounds); i++ {
		if DecisionBounds[i] <= DecisionBounds[i-1] {
			t.Fatalf("bound %d (%v) does not rise above %v", i, DecisionBounds[i],
				DecisionBounds[i-1])
		}
	}
	if DecisionBounds[0] != 60 || DecisionBounds[len(DecisionBounds)-1] != 7*24*3600 {
		t.Fatalf("bounds %v, want a minute to a week", DecisionBounds)
	}
}
