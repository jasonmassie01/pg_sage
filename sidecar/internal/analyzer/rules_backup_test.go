package analyzer

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-07: the WAL archiver rules. wal_archive_failing fires when
// failed_count rose and the last failure is after the last success (a
// streak of 30 minutes or more is critical); wal_archive_stale fires when
// archiving is on, completed WAL waits and the last archive is older than
// backup.max_archive_gap_minutes.

// No concurrent access tests: the rules are pure functions of two
// snapshots and the config, run on the analyzer goroutine.

var archT0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func fptr(f float64) *float64 { return &f }

// failingArchiver is an archiver that last archived at archT0 and last
// failed streak later, with failed failures.
func failingArchiver(failed int64, streak time.Duration) *collector.ArchiverStats {
	return &collector.ArchiverStats{ArchiveMode: "on", ArchivedCount: 40,
		FailedCount: failed, LastArchivedWAL: strPtr("000000010000000000000028"),
		LastArchivedTime: timePtr(archT0), LastFailedTime: timePtr(archT0.Add(streak)),
		StatsReset:         timePtr(archT0.Add(-24 * time.Hour)),
		ArchivedAgeSeconds: fptr(streak.Seconds()),
		CurrentWAL:         strPtr("00000001000000000000002B"), SegmentBytes: 16 << 20}
}

func archSnap(a *collector.ArchiverStats) *collector.Snapshot {
	return &collector.Snapshot{CollectedAt: archT0, Archiver: a}
}

func backupCfg() *config.Config {
	cfg := phase2Config()
	cfg.Backup = config.BackupConfig{MaxAgeHours: 26, MaxArchiveGapMinutes: 15,
		MinRetentionDays: 7}
	return cfg
}

func TestWALArchiveFailing_HappyPathWarning(t *testing.T) {
	prev, cur := archSnap(failingArchiver(3, 5*time.Minute)),
		archSnap(failingArchiver(6, 10*time.Minute))
	got := ruleWALArchiveFailing(cur, prev, backupCfg(), &RuleExtras{})
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one wal_archive_failing", got)
	}
	f := got[0]
	if f.Category != "wal_archive_failing" || f.Severity != "warning" ||
		f.ObjectIdentifier != "archiver" || f.ObjectType != "archiver" {
		t.Fatalf("finding = %+v", f)
	}
	if f.Detail["failed_since_last_cycle"] != int64(3) || f.Detail["failed_count"] != int64(6) {
		t.Fatalf("detail = %+v, want 3 new failures of 6", f.Detail)
	}
	if f.RecommendedSQL != "" {
		t.Fatalf("an archiver finding is advice only, got SQL %q", f.RecommendedSQL)
	}
}

// Boundary: a 29-minute failure streak is a warning, 30 minutes critical.
func TestWALArchiveFailing_StreakBoundary(t *testing.T) {
	for _, tc := range []struct {
		streak time.Duration
		want   string
	}{{29 * time.Minute, "warning"}, {30 * time.Minute, "critical"},
		{6 * time.Hour, "critical"}} {
		prev := archSnap(failingArchiver(3, tc.streak-time.Minute))
		cur := archSnap(failingArchiver(4, tc.streak))
		got := ruleWALArchiveFailing(cur, prev, backupCfg(), &RuleExtras{})
		if len(got) != 1 || got[0].Severity != tc.want {
			t.Errorf("streak %v: findings %+v, want one %s", tc.streak, got, tc.want)
		}
	}
}

// A failed_count that did not rise, or a success after the last failure,
// is no new failure.
func TestWALArchiveFailing_NoRiseOrRecoveredIsQuiet(t *testing.T) {
	steady := failingArchiver(6, 10*time.Minute)
	if got := ruleWALArchiveFailing(archSnap(steady), archSnap(steady), backupCfg(),
		&RuleExtras{}); len(got) != 0 {
		t.Fatalf("no new failures: %+v", got)
	}
	recovered := failingArchiver(7, 10*time.Minute)
	recovered.LastArchivedTime = timePtr(archT0.Add(11 * time.Minute))
	if got := ruleWALArchiveFailing(archSnap(recovered), archSnap(steady), backupCfg(),
		&RuleExtras{}); len(got) != 0 {
		t.Fatalf("archived after the last failure: %+v", got)
	}
}

// Nil/empty: archive_mode = off gives no archiver finding (AP-11 owns that
// case), even with old failure counters left over.
func TestWALArchiveRules_ArchiveModeOffIsQuiet(t *testing.T) {
	prev, cur := failingArchiver(3, time.Hour), failingArchiver(9, 2*time.Hour)
	prev.ArchiveMode, cur.ArchiveMode = "off", "off"
	cfg := backupCfg()
	if got := ruleWALArchiveFailing(archSnap(cur), archSnap(prev), cfg, nil); len(got) != 0 {
		t.Fatalf("archive_mode off: failing = %+v", got)
	}
	if got := ruleWALArchiveStale(archSnap(cur), nil, cfg, nil); len(got) != 0 {
		t.Fatalf("archive_mode off: stale = %+v", got)
	}
}

// "always" archives too (a standby's own archiving).
func TestWALArchiveFailing_ArchiveModeAlways(t *testing.T) {
	prev, cur := failingArchiver(3, time.Minute), failingArchiver(4, 2*time.Minute)
	prev.ArchiveMode, cur.ArchiveMode = "always", "always"
	if got := ruleWALArchiveFailing(archSnap(cur), archSnap(prev), backupCfg(),
		nil); len(got) != 1 {
		t.Fatalf("archive_mode always: %+v, want one finding", got)
	}
}

// Never archived since the reset: the streak runs from stats_reset; with
// neither time known it is a warning of unknown length.
func TestWALArchiveFailing_NeverArchived(t *testing.T) {
	prev, cur := failingArchiver(1, time.Minute), failingArchiver(2, 2*time.Minute)
	for _, a := range []*collector.ArchiverStats{prev, cur} {
		a.LastArchivedTime, a.LastArchivedWAL, a.ArchivedAgeSeconds = nil, nil, nil
	}
	got := ruleWALArchiveFailing(archSnap(cur), archSnap(prev), backupCfg(), nil)
	if len(got) != 1 || got[0].Severity != "critical" {
		t.Fatalf("failing since a reset a day ago: %+v, want critical", got)
	}
	prev.StatsReset, cur.StatsReset = nil, nil
	got = ruleWALArchiveFailing(archSnap(cur), archSnap(prev), backupCfg(), nil)
	if len(got) != 1 || got[0].Severity != "warning" || got[0].Detail["streak_known"] != false {
		t.Fatalf("streak unknown: %+v, want a warning marked streak_known=false", got)
	}
}

// The interval rule needs both snapshots' archiver rows from one stats
// epoch; anything else is no verdict (its open finding stays open).
func TestWALArchiveFailing_NeedsAnInterval(t *testing.T) {
	a := failingArchiver(3, time.Minute)
	reset := failingArchiver(1, 2*time.Minute)
	reset.StatsReset = timePtr(archT0)
	unread := archSnap(a)
	unread.Unavailable = map[string]string{"archiver": "permission denied"}
	cases := []struct {
		name      string
		cur, prev *collector.Snapshot
		want      bool
	}{
		{"both read", archSnap(a), archSnap(a), true},
		{"no previous snapshot", archSnap(a), nil, false},
		{"previous without archiver", archSnap(a), archSnap(nil), false},
		{"current without archiver", archSnap(nil), archSnap(a), false},
		{"read failed", unread, archSnap(a), false},
		{"stats reset between", archSnap(reset), archSnap(a), false},
	}
	for _, tc := range cases {
		if got := hasArchiverInterval(tc.cur, tc.prev); got != tc.want {
			t.Errorf("%s: hasArchiverInterval = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func staleArchiver(age time.Duration) *collector.ArchiverStats {
	return &collector.ArchiverStats{ArchiveMode: "on", ArchivedCount: 40,
		LastArchivedWAL:  strPtr("000000010000000000000028"),
		LastArchivedTime: timePtr(archT0), ArchivedAgeSeconds: fptr(age.Seconds()),
		CurrentWAL: strPtr("00000001000000000000002B"), SegmentBytes: 16 << 20}
}

func TestWALArchiveStale_HappyPathAndBoundary(t *testing.T) {
	cfg := backupCfg()
	if got := ruleWALArchiveStale(archSnap(staleArchiver(15*time.Minute)), nil, cfg,
		nil); len(got) != 0 {
		t.Fatalf("exactly the 15-minute gap fired: %+v", got)
	}
	got := ruleWALArchiveStale(archSnap(staleArchiver(16*time.Minute)), nil, cfg, nil)
	if len(got) != 1 || got[0].Category != "wal_archive_stale" ||
		got[0].Severity != "warning" || got[0].Detail["pending_segments"] != int64(2) {
		t.Fatalf("16 minutes, 2 completed segments waiting: %+v", got)
	}
	got = ruleWALArchiveStale(archSnap(staleArchiver(time.Hour)), nil, cfg, nil)
	if len(got) != 1 || got[0].Severity != "critical" {
		t.Fatalf("an hour (4x the gap): %+v, want critical", got)
	}
}

// No completed WAL since the last archive (an idle server) is not stale,
// however old the last archive is; an unknown position is no verdict.
func TestWALArchiveStale_NeedsWALToHaveAdvanced(t *testing.T) {
	cfg := backupCfg()
	idle := staleArchiver(48 * time.Hour)
	idle.CurrentWAL = strPtr("000000010000000000000029")
	if got := ruleWALArchiveStale(archSnap(idle), nil, cfg, nil); len(got) != 0 {
		t.Fatalf("only the segment being written waits: %+v", got)
	}
	standby := staleArchiver(48 * time.Hour)
	standby.CurrentWAL = nil
	if got := ruleWALArchiveStale(archSnap(standby), nil, cfg, nil); len(got) != 0 {
		t.Fatalf("unknown WAL position (standby): %+v", got)
	}
	history := staleArchiver(48 * time.Hour)
	history.LastArchivedWAL = strPtr("00000002.history")
	if got := ruleWALArchiveStale(archSnap(history), nil, cfg, nil); len(got) != 0 {
		t.Fatalf("a history file name is no segment position: %+v", got)
	}
	never := staleArchiver(48 * time.Hour)
	never.LastArchivedTime, never.ArchivedAgeSeconds, never.LastArchivedWAL = nil, nil, nil
	if got := ruleWALArchiveStale(archSnap(never), nil, cfg, nil); len(got) != 0 {
		t.Fatalf("never archived: %+v", got)
	}
}

// A failing archiver is reported by wal_archive_failing only.
func TestWALArchiveStale_FailingIsLeftToTheFailingRule(t *testing.T) {
	a := failingArchiver(9, 2*time.Hour)
	if got := ruleWALArchiveStale(archSnap(a), nil, backupCfg(), nil); len(got) != 0 {
		t.Fatalf("stale fired for a failing archiver: %+v", got)
	}
}

func TestPendingSegments(t *testing.T) {
	cases := []struct {
		last, cur string
		seg       int64
		want      int64
		ok        bool
	}{
		{"000000010000000000000028", "00000001000000000000002B", 16 << 20, 2, true},
		{"000000010000000000000028", "000000010000000000000029", 16 << 20, 0, true},
		// Across a log boundary: 0x100 segments of 16 MB per log file.
		{"0000000100000001000000FF", "000000010000000200000001", 16 << 20, 1, true},
		// 1 GB segments: 4 per log file.
		{"000000010000000100000003", "000000010000000200000001", 1 << 30, 1, true},
		{"000000010000000000000028.00000028.backup", "00000001000000000000002A",
			16 << 20, 1, true},
		{"00000002.history", "00000002000000000000002A", 16 << 20, 0, false},
		{"zz0000010000000000000028", "00000001000000000000002A", 16 << 20, 0, false},
		{"000000010000000000000028", "00000001000000000000002A", 0, 0, false},
	}
	for _, tc := range cases {
		a := &collector.ArchiverStats{LastArchivedWAL: strPtr(tc.last), CurrentWAL: strPtr(tc.cur),
			SegmentBytes: tc.seg}
		got, ok := pendingSegments(a)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s -> %s (%d): %d, %v; want %d, %v", tc.last, tc.cur, tc.seg, got, ok,
				tc.want, tc.ok)
		}
	}
	if _, ok := pendingSegments(nil); ok {
		t.Error("nil archiver row gave a position")
	}
}

// In fleet mode archiving is cluster-wide: a database that is not its
// instance's owner reports nothing, so the cluster is reported once.
func TestWALArchiveRules_DeferToTheInstanceOwner(t *testing.T) {
	prev, cur := archSnap(failingArchiver(3, time.Minute)),
		archSnap(failingArchiver(4, 2*time.Minute))
	deferring := &RuleExtras{ArchiverOwner: "orders"}
	if got := ruleWALArchiveFailing(cur, prev, backupCfg(), deferring); len(got) != 0 {
		t.Fatalf("a non-owner reported the failing archiver: %+v", got)
	}
	if got := ruleWALArchiveStale(archSnap(staleArchiver(time.Hour)), nil, backupCfg(),
		deferring); len(got) != 0 {
		t.Fatalf("a non-owner reported the stale archiver: %+v", got)
	}
	if got := ruleWALArchiveFailing(cur, prev, backupCfg(), &RuleExtras{}); len(got) != 1 {
		t.Fatalf("the owner reported %+v, want one finding", got)
	}
}

// State transitions across cycles: failing opens, a cycle where the
// archiver still has not succeeded (failed_count steady between archiver
// retries) keeps the open finding, a success resolves it.
func TestWALArchiveFailing_CycleKeepsUntilASuccess(t *testing.T) {
	a := New(nil, backupCfg(), nil, nil, nil, nil, nil, noopLog)
	first, second := failingArchiver(3, time.Minute), failingArchiver(5, 2*time.Minute)
	a.eval = newCycleEval()
	got := a.runSnapshotRules(archSnap(second), archSnap(first), true)
	if f := findingFor(got, "archiver"); f == nil || f.Category != "wal_archive_failing" {
		t.Fatalf("cycle 1: %+v, want wal_archive_failing", got)
	}
	a.eval = newCycleEval()
	a.runSnapshotRules(archSnap(second), archSnap(second), true)
	if !a.eval.kept("wal_archive_failing")["archiver"] {
		t.Fatal("cycle 2: still failing (no success since) but the finding may resolve")
	}
	recovered := failingArchiver(5, 2*time.Minute)
	recovered.LastArchivedTime = timePtr(archT0.Add(3 * time.Minute))
	a.eval = newCycleEval()
	a.runSnapshotRules(archSnap(recovered), archSnap(second), true)
	if a.eval.kept("wal_archive_failing")["archiver"] ||
		!a.eval.resolvable(nil)["wal_archive_failing"] {
		t.Fatal("cycle 3: archived again, the finding must resolve")
	}
	a.eval = newCycleEval()
	a.runSnapshotRules(archSnap(nil), archSnap(second), true)
	if a.eval.resolvable(nil)["wal_archive_failing"] ||
		a.eval.resolvable(nil)["wal_archive_stale"] {
		t.Fatal("archiver unread: its categories claimed evaluated")
	}
}
