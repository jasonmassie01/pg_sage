package analyzer

import (
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// collection-04 review: a key on a partitioned table is enforced on its
// partitions, so the protection of their supporting indexes from
// unused-index drops must not depend on what the snapshot happens to
// hold. A partition missing from Tables is still protected, and when the
// partition list could not be read the drop rule does not run for keys it
// cannot place (fail closed).
//
// No concurrency test: both are pure functions of one snapshot.

func TestBuildFKRequirements_PartitionsMissingFromTablesStayProtected(t *testing.T) {
	snap := partitionedEvents()
	snap.Tables = nil // tables unavailable: no kinds are known
	got := buildFKRequirements(snap)
	cols := [][]string{{"customer_id"}}
	want := map[tableKey][][]string{{"app", "events"}: cols, {"app", "events_p0"}: cols,
		{"app", "events_p1"}: cols, {"app", "events_p1a"}: cols}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %v, want %v", got, want)
	}
	extras := &RuleExtras{FirstSeen: map[string]time.Time{
		"app.events_p0_cust": time.Now().Add(-30 * 24 * time.Hour),
	}, RecentlyCreated: map[string]time.Time{}}
	if got := ruleUnusedIndexes(snap, nil, unusedCfg(), extras); len(got) != 0 {
		t.Fatalf("sole FK index of a partition recommended for drop: %+v", got)
	}
}

func TestHasIndexes_PartitionsUnreadFailsClosedForUnplacedKeys(t *testing.T) {
	unread := map[string]string{"partitions": "statement timeout"}
	cases := []struct {
		name  string
		edit  func(*collector.Snapshot)
		ready bool
	}{
		{"partitions read", func(*collector.Snapshot) {}, true},
		{"partitioned key, partitions unread",
			func(s *collector.Snapshot) { s.Unavailable = unread }, false},
		{"key on a table missing from Tables, partitions unread",
			func(s *collector.Snapshot) {
				s.Unavailable = unread
				s.ForeignKeys[0].TableName = "gone"
			}, false},
		{"key on a plain table, partitions unread",
			func(s *collector.Snapshot) {
				s.Unavailable = unread
				s.ForeignKeys[0].TableName = "events_p0"
			}, true},
		{"no keys, partitions unread",
			func(s *collector.Snapshot) {
				s.Unavailable = unread
				s.ForeignKeys = []collector.ForeignKey{}
			}, true},
	}
	for _, tc := range cases {
		snap := partitionedEvents()
		tc.edit(snap)
		if got := hasIndexes(snap, nil); got != tc.ready {
			t.Errorf("%s: hasIndexes = %v, want %v", tc.name, got, tc.ready)
		}
	}
}
