package schemaguard

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCloneStemNeedsAGeneratedSuffix(t *testing.T) {
	cases := map[string]string{
		"test_memory_a1b2c3d4": "test_memory_",
		"tenant_000123":        "tenant_",
		"run-2026-10-02":       "run-",
		"public":               "",
		"orders2024":           "",
		"abcdef123456":         "",
		"tenant_abcdef":        "",
		"":                     "",
	}
	for name, want := range cases {
		if got := CloneStem(name); got != want {
			t.Errorf("CloneStem(%q) = %q, want %q", name, got, want)
		}
	}
}

func shapes(prefix string, n int, tables ...string) []SchemaShape {
	result := make([]SchemaShape, 0, n)
	for i := 1; i <= n; i++ {
		result = append(result, SchemaShape{
			Schema: fmt.Sprintf("%s%06d", prefix, i), Tables: tables})
	}
	return result
}

func TestGroupFamiliesNeedsFiveCopiesOfOneShape(t *testing.T) {
	input := append(shapes("tenant_", 5, "orders", "items"),
		shapes("scratch_", 4, "notes")...)
	input = append(input, SchemaShape{Schema: "public", Tables: []string{"orders", "items"}})
	families := GroupFamilies(input)
	if len(families) != 5 {
		t.Fatalf("family members = %d (%v), want the 5 tenants only", len(families), families)
	}
	family := families["tenant_000003"]
	if family == nil || len(family.Members) != 5 ||
		!strings.HasPrefix(family.Key, "tenant_*:") || family.Members[0] != "tenant_000001" {
		t.Fatalf("tenant family = %+v, want 5 sorted members keyed by stem", family)
	}
	if families["tenant_000001"] != family {
		t.Fatal("members of one family do not share one Family")
	}
	if families["public"] != nil || families["scratch_000001"] != nil {
		t.Fatal("a non-suffixed schema or a 4-copy group became a family")
	}
}

func TestGroupFamiliesSeparatesShapesAndIgnoresOrder(t *testing.T) {
	input := append(shapes("tenant_", 5, "orders", "items"),
		shapes("tenant_9", 5, "ledger")...)
	families := GroupFamilies(input)
	first, second := families["tenant_000001"], families["tenant_9000001"]
	if first == nil || second == nil || first.Key == second.Key {
		t.Fatalf("families = %+v / %+v, want two distinct shapes", first, second)
	}
	reordered := shapes("tenant_", 5, "items", "orders")
	if got := GroupFamilies(reordered)["tenant_000001"]; got == nil || got.Key != first.Key {
		t.Fatalf("table order changed the family key: %+v vs %+v", got, first)
	}
}

func TestGroupFamiliesEmptyInput(t *testing.T) {
	if got := GroupFamilies(nil); len(got) != 0 {
		t.Fatalf("GroupFamilies(nil) = %v", got)
	}
	if got := GroupFamilies(shapes("tenant_", 5)); len(got) != 0 {
		t.Fatalf("schemas without tables formed a family: %v", got)
	}
}

func TestIdleTrackerMeasuresUnchangedActivity(t *testing.T) {
	tracker := NewIdleTracker()
	start := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	if quiet := tracker.QuietFor("f", 10, start); quiet != 0 {
		t.Fatalf("first sight quiet = %v, want 0", quiet)
	}
	if quiet := tracker.QuietFor("f", 10, start.Add(time.Hour)); quiet != time.Hour {
		t.Fatalf("unchanged quiet = %v, want 1h", quiet)
	}
	if quiet := tracker.QuietFor("f", 11, start.Add(2*time.Hour)); quiet != 0 {
		t.Fatalf("changed activity quiet = %v, want 0", quiet)
	}
	tracker.Retain(map[string]bool{})
	if quiet := tracker.QuietFor("f", 11, start.Add(3*time.Hour)); quiet != 0 {
		t.Fatalf("pruned family kept its mark: quiet = %v", quiet)
	}
	var nilTracker *IdleTracker
	if quiet := nilTracker.QuietFor("f", 1, start); quiet != 0 {
		t.Fatalf("nil tracker quiet = %v", quiet)
	}
}

func TestIdleTrackerConcurrentUse(t *testing.T) {
	tracker := NewIdleTracker()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("f%d", i%2)
			tracker.QuietFor(key, int64(i), time.Now())
			tracker.Retain(map[string]bool{key: true})
		}(i)
	}
	wg.Wait()
}

func TestClassifyIdleRequiresEveryQuietSignal(t *testing.T) {
	week := 7 * 24 * time.Hour
	none := map[string]bool{}
	cases := []struct {
		name     string
		evidence IdleEvidence
		idle     bool
		reason   string
	}{
		{"never used", IdleEvidence{Window: week, StatementSchemas: none,
			SessionSchemas: none}, true, "no scans or writes"},
		{"recently written", IdleEvidence{Activity: 5, QuietFor: time.Hour, Window: week,
			StatementSchemas: none, SessionSchemas: none}, false, "scanned or written"},
		{"quiet for the window", IdleEvidence{Activity: 5, QuietFor: week, Window: week,
			StatementSchemas: none, SessionSchemas: none}, true, "no scans or writes"},
		{"statement names a member", IdleEvidence{Window: week,
			StatementSchemas: map[string]bool{"tenant_000002": true},
			SessionSchemas:   none}, false, "pg_stat_statements"},
		{"statements unknown", IdleEvidence{Window: week, SessionSchemas: none},
			false, "statement activity is unknown"},
		{"sessions unknown", IdleEvidence{Window: week, StatementSchemas: none},
			false, "session activity is unknown"},
		{"session uses a member", IdleEvidence{Window: week, StatementSchemas: none,
			SessionSchemas: map[string]bool{"tenant_000004": true}}, false, "session"},
	}
	for _, tc := range cases {
		family := &Family{Key: "tenant_*:00", Members: familyMembers(5)}
		ClassifyIdle(family, tc.evidence)
		if family.Idle != tc.idle || !strings.Contains(family.Reason, tc.reason) {
			t.Errorf("%s: idle=%v reason=%q, want idle=%v reason containing %q",
				tc.name, family.Idle, family.Reason, tc.idle, tc.reason)
		}
	}
	ClassifyIdle(nil, IdleEvidence{})
}
