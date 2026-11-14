package advisor

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
)

// market-15: on PostgreSQL 18 the advisor reports I/O settings carried over
// from an older server (a pinned effective_io_concurrency or
// maintenance_io_concurrency at or below the pre-18 default, or io_method =
// sync), as advice with no SQL.

// No concurrent access or state-transition tests: the check is a pure
// function over one snapshot and runs once per advisor cycle.
// No error propagation tests: the check reads the snapshot only; a failed
// collection leaves ConfigData nil, covered by the nil cases below.

func ioSnap(settings ...collector.PGSetting) *collector.ConfigSnapshot {
	return &collector.ConfigSnapshot{PGSettings: settings}
}

func ioSetting(name, value, source string) collector.PGSetting {
	return collector.PGSetting{Name: name, Setting: value, Source: source,
		Context: "user"}
}

func TestIOSettings_CarriedOverEffectiveIOConcurrency(t *testing.T) {
	got := ioSettingsFindings(ioSnap(
		ioSetting("effective_io_concurrency", "1", "configuration file"),
		ioSetting("work_mem", "4096", "configuration file")), 18)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.Category != "io_settings" || f.Severity != "info" ||
		f.ObjectType != "configuration" ||
		f.ObjectIdentifier != "instance:effective_io_concurrency" {
		t.Fatalf("finding = %+v, want an info io_settings finding on the instance setting", f)
	}
	if !strings.Contains(f.Title, "effective_io_concurrency") ||
		!strings.Contains(f.Title, "16") {
		t.Errorf("title %q does not name the setting and the new default", f.Title)
	}
	if f.RecommendedSQL != "" || f.RollbackSQL != "" {
		t.Errorf("advice-only finding carries SQL %q / %q", f.RecommendedSQL, f.RollbackSQL)
	}
	d := f.Detail
	if d["setting"] != "effective_io_concurrency" || d["value"] != "1" ||
		d["source"] != "configuration file" || d["pg_major"] != 18 ||
		d["pg18_default"] != "16" || d["pre18_default"] != "1" {
		t.Errorf("detail = %v, want setting, value, source, major and defaults", d)
	}
	note, _ := d["doc_note"].(string)
	if !strings.Contains(note, "PostgreSQL 18 raised the default") {
		t.Errorf("doc_note = %q, want the PG18 version note", note)
	}
	if !strings.Contains(f.Recommendation, "configuration file") {
		t.Errorf("recommendation %q does not name where the value comes from",
			f.Recommendation)
	}
}

func TestIOSettings_MaintenanceIOConcurrencyBoundary(t *testing.T) {
	at := ioSettingsFindings(ioSnap(
		ioSetting("maintenance_io_concurrency", "10", "configuration file")), 18)
	if len(at) != 1 || at[0].ObjectIdentifier != "instance:maintenance_io_concurrency" ||
		at[0].Detail["pre18_default"] != "10" {
		t.Fatalf("maintenance_io_concurrency=10: %+v, want one finding", at)
	}
	note, _ := at[0].Detail["doc_note"].(string)
	if !strings.Contains(note, "10") {
		t.Errorf("doc_note = %q, want the advice-only PG18 note", note)
	}
	above := ioSettingsFindings(ioSnap(
		ioSetting("maintenance_io_concurrency", "11", "configuration file")), 18)
	if len(above) != 0 {
		t.Errorf("maintenance_io_concurrency=11: %+v, want none", above)
	}
}

func TestIOSettings_Boundaries(t *testing.T) {
	cases := []struct {
		name    string
		setting collector.PGSetting
		major   int
		want    int
	}{
		{"value 2 is a choice, not a carry-over",
			ioSetting("effective_io_concurrency", "2", "configuration file"), 18, 0},
		{"value 1 from the default source",
			ioSetting("effective_io_concurrency", "1", "default"), 18, 0},
		{"PostgreSQL 17 keeps the old default",
			ioSetting("effective_io_concurrency", "1", "configuration file"), 17, 0},
		{"value 0 switches prefetch off",
			ioSetting("effective_io_concurrency", "0", "configuration file"), 18, 1},
		{"a later major still applies",
			ioSetting("effective_io_concurrency", "1", "configuration file"), 19, 1},
		{"set per database counts as carried",
			ioSetting("effective_io_concurrency", "1", "database"), 18, 1},
		{"io_method sync from a file",
			ioSetting("io_method", "sync", "configuration file"), 18, 1},
		{"io_method sync from the command line",
			ioSetting("io_method", "SYNC", "command line"), 18, 1},
		{"io_method sync as the default",
			ioSetting("io_method", "sync", "default"), 18, 0},
		{"io_method worker from a file",
			ioSetting("io_method", "worker", "configuration file"), 18, 0},
		{"io_uring is asynchronous",
			ioSetting("io_method", "io_uring", "configuration file"), 18, 0},
		{"io_workers is never reported",
			ioSetting("io_workers", "1", "configuration file"), 18, 0},
	}
	for _, c := range cases {
		got := ioSettingsFindings(ioSnap(c.setting), c.major)
		if len(got) != c.want {
			t.Errorf("%s: %d findings %+v, want %d", c.name, len(got), got, c.want)
		}
	}
}

func TestIOSettings_IOMethodSync(t *testing.T) {
	got := ioSettingsFindings(ioSnap(ioSetting("io_method", "sync", "configuration file")), 18)
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want one", got)
	}
	f := got[0]
	if f.ObjectIdentifier != "instance:io_method" || f.RecommendedSQL != "" ||
		!strings.Contains(f.Title, "asynchronous I/O is switched off") {
		t.Errorf("finding = %+v, want the io_method advice", f)
	}
	note, _ := f.Detail["doc_note"].(string)
	if !strings.Contains(note, "io_uring") || f.Detail["source"] != "configuration file" {
		t.Errorf("detail = %v, want the io_method note and source", f.Detail)
	}
	if !strings.Contains(f.Recommendation, "restart") {
		t.Errorf("recommendation %q does not say io_method needs a restart", f.Recommendation)
	}
}

func TestIOSettings_InvalidValueSkipped(t *testing.T) {
	got := ioSettingsFindings(ioSnap(
		ioSetting("effective_io_concurrency", "abc", "configuration file"),
		ioSetting("maintenance_io_concurrency", "", "configuration file"),
		ioSetting("effective_io_concurrency", "-1", "configuration file"),
		ioSetting("io_method", "", "configuration file")), 18)
	if len(got) != 0 {
		t.Fatalf("findings for invalid values = %+v, want none", got)
	}
}

func TestIOSettings_NilEmptyAndUnknownMajor(t *testing.T) {
	if got := ioSettingsFindings(nil, 18); got != nil {
		t.Errorf("nil config: %+v", got)
	}
	if got := ioSettingsFindings(ioSnap(), 18); got != nil {
		t.Errorf("empty settings: %+v", got)
	}
	carried := ioSnap(ioSetting("effective_io_concurrency", "1", "configuration file"))
	for _, major := range []int{0, -1} {
		if got := ioSettingsFindings(carried, major); got != nil {
			t.Errorf("major %d: %+v, want none", major, got)
		}
	}
}

// Both carried-over settings give one finding each, in snapshot order.
func TestIOSettings_BothSettingsInOrder(t *testing.T) {
	got := ioSettingsFindings(ioSnap(
		ioSetting("maintenance_io_concurrency", "10", "configuration file"),
		ioSetting("io_method", "sync", "configuration file"),
		ioSetting("effective_io_concurrency", "1", "configuration file")), 18)
	var ids []string
	for _, f := range got {
		ids = append(ids, f.ObjectIdentifier)
	}
	want := []string{"instance:maintenance_io_concurrency", "instance:io_method",
		"instance:effective_io_concurrency"}
	if !slices.Equal(ids, want) {
		t.Fatalf("identifiers = %v, want %v", ids, want)
	}
}

func ioCycleSnapshot() *collector.Snapshot {
	return &collector.Snapshot{ConfigData: ioSnap(
		ioSetting("effective_io_concurrency", "1", "configuration file"))}
}

func ioFindings(fs []analyzer.Finding) []analyzer.Finding {
	var out []analyzer.Finding
	for _, f := range fs {
		if f.Category == "io_settings" {
			out = append(out, f)
		}
	}
	return out
}

// Integration with the cycle: the check runs with the LLM off, and the
// category counts as evaluated so a fixed setting's finding resolves.
func TestIOSettings_AdvisorCycle(t *testing.T) {
	a := newTestAdvisor(true, false, 0)
	a.WithPGVersion(180001)
	got := ioFindings(a.analyzeSnapshot(context.Background(), ioCycleSnapshot()))
	if len(got) != 1 || got[0].ObjectIdentifier != "instance:effective_io_concurrency" {
		t.Fatalf("cycle io findings = %+v, want the carried-over setting", got)
	}
	if !slices.Contains(a.LastEvaluatedCategories(), "io_settings") {
		t.Errorf("evaluated = %v, want io_settings", a.LastEvaluatedCategories())
	}
}

func TestIOSettings_AdvisorCycleByMajor(t *testing.T) {
	cases := []struct {
		version   int
		evaluated bool
	}{{170004, true}, {140010, true}, {0, false}}
	for _, c := range cases {
		a := newTestAdvisor(true, false, 0)
		a.WithPGVersion(c.version)
		got := ioFindings(a.analyzeSnapshot(context.Background(), ioCycleSnapshot()))
		if len(got) != 0 {
			t.Errorf("version %d: io findings %+v, want none", c.version, got)
		}
		if ev := slices.Contains(a.LastEvaluatedCategories(), "io_settings"); ev != c.evaluated {
			t.Errorf("version %d: io_settings evaluated = %v, want %v", c.version, ev,
				c.evaluated)
		}
	}
}

// A database that leaves its instance settings to another database in the
// fleet does not report the instance-wide values: the owner does. It still
// evaluates the category, so its own stale instance rows (from when it was
// the owner) resolve rather than duplicate the owner's.
func TestIOSettings_NonOwnerDefersInstanceValues(t *testing.T) {
	a := newTestAdvisor(true, false, 0)
	a.WithPGVersion(180001)
	a.WithInstanceOwner(func() (string, bool) { return "owner_db", false })
	got := ioFindings(a.analyzeSnapshot(context.Background(), ioCycleSnapshot()))
	if len(got) != 0 {
		t.Fatalf("non-owner io findings = %+v, want none", got)
	}
	if !slices.Contains(a.LastEvaluatedCategories(), "io_settings") {
		t.Errorf("non-owner did not evaluate io_settings: %v", a.LastEvaluatedCategories())
	}
}

// A value set for one database alone (ALTER DATABASE, or ALTER ROLE ... IN
// DATABASE) is seen only from that database, so every database reports
// its own, owner or not (plan: fleet per database).
func TestIOSettings_DatabaseOverridePerDatabase(t *testing.T) {
	for _, source := range []string{"database", "database user"} {
		for _, isOwner := range []bool{true, false} {
			a := newTestAdvisor(true, false, 0)
			a.WithPGVersion(180001)
			a.WithInstanceOwner(func() (string, bool) { return "owner_db", isOwner })
			snap := &collector.Snapshot{ConfigData: ioSnap(
				ioSetting("effective_io_concurrency", "1", source),
				ioSetting("io_method", "sync", "configuration file"))}
			got := ioFindings(a.analyzeSnapshot(context.Background(), snap))
			var ids []string
			for _, f := range got {
				ids = append(ids, f.ObjectIdentifier)
			}
			want := []string{"database:effective_io_concurrency"}
			if isOwner {
				want = append(want, "instance:io_method")
			}
			if !slices.Equal(ids, want) {
				t.Errorf("source %q, owner %v: identifiers %v, want %v", source, isOwner,
					ids, want)
				continue
			}
			if got[0].Detail["scope"] != "database" || got[0].Detail["source"] != source {
				t.Errorf("source %q: detail %v, want database scope and the source",
					source, got[0].Detail)
			}
		}
	}
}

// Instance-wide sources keep the instance scope.
func TestIOSettings_InstanceScopeSources(t *testing.T) {
	for _, source := range []string{"configuration file", "command line", "user", "global"} {
		got := ioSettingsFindings(ioSnap(ioSetting("effective_io_concurrency", "1",
			source)), 18)
		if len(got) != 1 || got[0].ObjectIdentifier != "instance:effective_io_concurrency" ||
			got[0].Detail["scope"] != "instance" {
			t.Errorf("source %q: %+v, want one instance-scope finding", source, got)
		}
	}
}

// Without a config snapshot the category is not evaluated, so an open
// finding is kept rather than resolved on missing evidence.
func TestIOSettings_AdvisorCycleWithoutSettings(t *testing.T) {
	a := newTestAdvisor(true, false, 0)
	a.WithPGVersion(180001)
	for _, snap := range []*collector.Snapshot{nil, {},
		{ConfigData: &collector.ConfigSnapshot{}}} {
		got := ioFindings(a.analyzeSnapshot(context.Background(), snap))
		if len(got) != 0 || slices.Contains(a.LastEvaluatedCategories(), "io_settings") {
			t.Errorf("snapshot %+v: io findings %+v, evaluated %v", snap, got,
				a.LastEvaluatedCategories())
		}
	}
}
