package collector

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

// market-07: the archiver step reads one row from pg_stat_archiver with
// the server's WAL position and segment size, and a failed read leaves the
// category unknown rather than empty.

var walFileName = regexp.MustCompile(`^[0-9A-F]{24}$`)

func TestCollect_ArchiverRowOnLiveServer(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if reason, missing := snap.Unavailable["archiver"]; missing {
		t.Fatalf("archiver unavailable: %s", reason)
	}
	a := snap.Archiver
	if a == nil {
		t.Fatal("archiver row not collected")
	}
	var mode string
	var segment int64
	var recovery bool
	if err := pool.QueryRow(ctx, `SELECT current_setting('archive_mode'),
		(SELECT setting::int8 FROM pg_settings WHERE name = 'wal_segment_size'),
		pg_is_in_recovery()`).Scan(&mode, &segment, &recovery); err != nil {
		t.Fatalf("read the server's settings: %v", err)
	}
	if a.ArchiveMode != mode || a.SegmentBytes != segment || a.SegmentBytes < 1<<20 {
		t.Fatalf("archiver = %+v, want mode %q and segment %d bytes", a, mode, segment)
	}
	if a.ArchivedCount < 0 || a.FailedCount < 0 {
		t.Fatalf("archiver counters = %d, %d", a.ArchivedCount, a.FailedCount)
	}
	if recovery {
		if a.CurrentWAL != nil {
			t.Fatalf("a standby reported a current WAL file %q", *a.CurrentWAL)
		}
	} else if a.CurrentWAL == nil || !walFileName.MatchString(*a.CurrentWAL) {
		t.Fatalf("current WAL = %v, want a 24-hex-digit file name", a.CurrentWAL)
	}
	if (a.LastArchivedTime == nil) != (a.ArchivedAgeSeconds == nil) {
		t.Fatalf("archived time %v and its age %v disagree", a.LastArchivedTime,
			a.ArchivedAgeSeconds)
	}
	requireStoredCategory(t, snap, "archiver")
}

func requireStoredCategory(t *testing.T, snap *Snapshot, category string) {
	t.Helper()
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatalf("snapshot rows: %v", err)
	}
	for _, r := range rows {
		if r.Category == category {
			return
		}
	}
	t.Fatalf("the %s category is not stored with the snapshot", category)
}

// Error propagation: a failed archiver read is unknown (nil and marked
// unavailable with the reason), never an empty archiver; the rest of the
// snapshot is kept.
func TestCollect_ArchiverReadFailureIsUnknown(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	c.overrideStep("archiver", func(_ context.Context, s *Snapshot) error {
		s.Archiver = &ArchiverStats{ArchiveMode: "on"}
		return errors.New("permission denied for view pg_stat_archiver")
	})
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect failed for one extra category: %v", err)
	}
	if snap.Archiver != nil || snap.Available("archiver") {
		t.Fatalf("failed read left archiver = %+v, available = %v", snap.Archiver,
			snap.Available("archiver"))
	}
	if reason := snap.Unavailable["archiver"]; reason == "" {
		t.Fatal("the unavailable archiver carries no reason")
	}
	if len(snap.Tables) == 0 && !snap.Available("tables") {
		t.Fatal("the archiver failure dropped the rest of the snapshot")
	}
}
