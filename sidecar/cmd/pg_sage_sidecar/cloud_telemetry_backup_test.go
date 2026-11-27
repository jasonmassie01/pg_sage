package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-07: backup_stale and backup_retention_below_policy from the
// provider's backup posture. Unknown evidence leaves a category
// unevaluated (its open finding stays), never resolved and never "ok".

var backupNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func backupPolicy() config.BackupConfig {
	return config.BackupConfig{MaxAgeHours: 26, MaxArchiveGapMinutes: 15, MinRetentionDays: 7}
}

func ago(d time.Duration) *time.Time {
	t := backupNow.Add(-d)
	return &t
}

func intp(v int) *int { return &v }

func TestBackupStaleFromTheRDSRestorableTime(t *testing.T) {
	b := &cloudtel.BackupPosture{LatestRestorableTime: ago(30 * time.Hour),
		RetentionDays: intp(7)}
	got := backupFindings(b, "rds:us-east-1/orders", backupPolicy(), backupNow)
	stale, ok := got[categoryBackupStale]
	if !ok || len(stale) != 1 {
		t.Fatalf("findings = %+v, want one backup_stale", got)
	}
	f := stale[0]
	if f.Severity != "warning" || f.ObjectIdentifier != "backup:rds:us-east-1/orders" ||
		f.Detail["age_hours"] != 30.0 || f.Detail["max_age_hours"] != 26 ||
		f.Detail["source"] != "latest_restorable_time" {
		t.Fatalf("backup_stale = %+v", f)
	}
	if !strings.Contains(f.Title, "30 hours") {
		t.Fatalf("title %q does not give the age", f.Title)
	}
	if retention, ok := got[categoryBackupRetention]; !ok || len(retention) != 0 {
		t.Fatalf("retention 7 against a 7-day policy: %+v, want evaluated and clear",
			retention)
	}
}

// Boundary: exactly max_age_hours does not fire; twice the age is
// critical.
func TestBackupStaleBoundaries(t *testing.T) {
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{{26 * time.Hour, ""}, {26*time.Hour + time.Minute, "warning"},
		{52 * time.Hour, "critical"}, {time.Hour, ""}} {
		b := &cloudtel.BackupPosture{LatestRestorableTime: ago(tc.age)}
		got := backupFindings(b, "rds:r/i", backupPolicy(), backupNow)
		stale, evaluated := got[categoryBackupStale]
		if !evaluated {
			t.Fatalf("age %v: backup_stale not evaluated with a known time", tc.age)
		}
		switch {
		case tc.want == "" && len(stale) != 0:
			t.Errorf("age %v fired: %+v", tc.age, stale)
		case tc.want != "" && (len(stale) != 1 || stale[0].Severity != tc.want):
			t.Errorf("age %v: %+v, want one %s", tc.age, stale, tc.want)
		}
	}
}

// Nil/empty: no restorable time (or no posture) is unknown, not stale and
// not fresh: the category is not evaluated, so an open finding stays.
func TestBackupFreshnessUnknownIsNotEvaluated(t *testing.T) {
	for name, b := range map[string]*cloudtel.BackupPosture{
		"nil posture":         nil,
		"no restorable time":  {RetentionDays: intp(7)},
		"freshness withheld":  {FreshnessUnavailable: "backupRuns.list HTTP 403"},
		"future restore time": {LatestRestorableTime: ago(-2 * time.Hour)},
	} {
		got := backupFindings(b, "cloud-sql:p:main", backupPolicy(), backupNow)
		if _, evaluated := got[categoryBackupStale]; evaluated {
			t.Errorf("%s: backup_stale evaluated as %+v", name, got[categoryBackupStale])
		}
	}
}

// Cloud SQL: failed runs and no success among the latest 5 is stale with
// an unknown age.
func TestBackupStaleWhenNoRecentRunSucceeded(t *testing.T) {
	b := &cloudtel.BackupPosture{NoRecentSuccess: true, BackupsEnabled: boolp(true)}
	stale := backupFindings(b, "cloud-sql:p:main", backupPolicy(), backupNow)[categoryBackupStale]
	if len(stale) != 1 || stale[0].Severity != "warning" ||
		stale[0].Detail["source"] != "backup_runs" {
		t.Fatalf("no successful run: %+v, want one warning from backup_runs", stale)
	}
}

func boolp(v bool) *bool { return &v }

// Retention against backup.min_retention_days: below fires, equal does
// not, 0 days is AP-11's "backups off" case, a 0 policy turns it off and
// an unknown retention is not evaluated.
func TestBackupRetentionBelowPolicy(t *testing.T) {
	policy := backupPolicy()
	cases := []struct {
		name      string
		retention *int
		min       int
		fires     bool
		evaluated bool
	}{
		{"3 < 7", intp(3), 7, true, true},
		{"6 < 7", intp(6), 7, true, true},
		{"7 = 7", intp(7), 7, false, true},
		{"0 days (AP-11)", intp(0), 7, false, true},
		{"policy off", intp(1), 0, false, true},
		{"unknown retention", nil, 7, false, false},
	}
	for _, tc := range cases {
		policy.MinRetentionDays = tc.min
		b := &cloudtel.BackupPosture{RetentionDays: tc.retention}
		got, evaluated := backupFindings(b, "rds:r/i", policy, backupNow)[categoryBackupRetention]
		if evaluated != tc.evaluated || (len(got) == 1) != tc.fires {
			t.Errorf("%s: findings %+v evaluated %v, want fires %v evaluated %v", tc.name,
				got, evaluated, tc.fires, tc.evaluated)
		}
		if tc.fires && (got[0].Severity != "warning" ||
			got[0].Detail["min_retention_days"] != tc.min) {
			t.Errorf("%s: finding %+v", tc.name, got[0])
		}
	}
}

// Error propagation: a withheld freshness (Cloud SQL backupRuns 403) is
// logged once per reason, not every poll; a new reason, or the same one
// after it cleared, is logged again.
func TestBackupFreshnessReasonLoggedOnce(t *testing.T) {
	var l reasonLatch
	steps := []struct {
		reason string
		log    bool
	}{{"backupRuns HTTP 403", true}, {"backupRuns HTTP 403", false}, {"", false},
		{"backupRuns HTTP 403", true}, {"backupRuns HTTP 429", true}}
	for i, s := range steps {
		if got := l.changed(s.reason); got != s.log {
			t.Fatalf("step %d (%q): changed = %v, want %v", i, s.reason, got, s.log)
		}
	}
}

// Concurrent access: the latch is shared by a runtime's managed cycles
// and must log a reason once however many goroutines see it.
func TestBackupReasonLatchConcurrent(t *testing.T) {
	var l reasonLatch
	var wg sync.WaitGroup
	var mu sync.Mutex
	logged := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.changed("denied") {
				mu.Lock()
				logged++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if logged != 1 {
		t.Fatalf("logged %d times, want once", logged)
	}
}
