package upgradecheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestTargetValidate(t *testing.T) {
	cases := []struct {
		name   string
		target Target
		source int
		ok     bool
	}{
		{"next major", Target{Major: 15}, 14, true},
		{"newest known", Target{Major: 18, Method: BlueGreen}, 14, true},
		{"same major", Target{Major: 16}, 16, false},
		{"older major", Target{Major: 15}, 16, false},
		{"newer than known", Target{Major: 19}, 17, false},
		{"zero", Target{}, 14, false},
		{"negative", Target{Major: -18}, 14, false},
		{"unknown method", Target{Major: 18, Method: "dump_restore"}, 14, false},
		{"case matters", Target{Major: 18, Method: "PG_UPGRADE"}, 14, false},
	}
	for _, c := range cases {
		err := c.target.Validate(c.source)
		if (err == nil) != c.ok {
			t.Fatalf("%s: Validate(%d) = %v, want ok=%v", c.name, c.source, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("%s: error %v is not ErrInvalidTarget", c.name, err)
		}
	}
	if got := (Target{Major: 18}).Normalized().Method; got != PGUpgrade {
		t.Fatalf("default method = %q, want pg_upgrade", got)
	}
	err := Target{Major: 19}.ValidateStatic()
	if err == nil || !strings.Contains(err.Error(), "up to 18") {
		t.Fatalf("too-new target error = %v, want it to name the newest known major", err)
	}
}

func TestRun_NilPool(t *testing.T) {
	_, err := Run(context.Background(), nil, Target{Major: 18}, Options{})
	if !errors.Is(err, ErrNoPool) {
		t.Fatalf("Run(nil pool) = %v, want ErrNoPool", err)
	}
	if _, err := NewMonitor(nil, MonitorOptions{}).Detect(context.Background()); !errors.Is(
		err, ErrNoPool) {
		t.Fatalf("Monitor(nil pool).Detect = %v, want ErrNoPool", err)
	}
}

func TestWorst(t *testing.T) {
	if worst(Pass, Unknown) != Unknown || worst(Block, Warn) != Block ||
		worst(Warn, Unknown) != Warn || worst(Pass, Pass) != Pass {
		t.Fatal("worst does not order pass < unknown < warn < block")
	}
}

func TestCompareExtensions(t *testing.T) {
	installed := []extension{{"pg_trgm", "1.6"}, {"vector", "0.8.2"}, {"postgis", "3.4.0"}}
	got := compareExtensions(installed, map[string][]string{
		"pg_trgm": {"1.5", "1.6"}, "vector": {"0.8.0", "0.8.1"}})
	if got.Status != Block || got.Total != 2 || len(got.Rows) != 2 {
		t.Fatalf("compare = %+v, want block with vector and postgis", got)
	}
	byName := map[string]Row{}
	for _, r := range got.Rows {
		byName[r["extension"]] = r
	}
	if byName["postgis"]["target"] != "not available" {
		t.Fatalf("postgis row = %v, want not available", byName["postgis"])
	}
	if byName["vector"]["target"] != "offers 0.8.0, 0.8.1" {
		t.Fatalf("vector row = %v, want the offered versions", byName["vector"])
	}
	if _, listed := byName["pg_trgm"]; listed {
		t.Fatal("an extension the target offers at its version must not be listed")
	}
	warn := compareExtensions(installed[1:2], map[string][]string{"vector": {"0.8.1"}})
	if warn.Status != Warn || !strings.Contains(warn.Reason, "ALTER EXTENSION") {
		t.Fatalf("version-only gap = %+v, want warn with the update advice", warn)
	}
	pass := compareExtensions(installed[:1], map[string][]string{"pg_trgm": {"1.6"}})
	if pass.Status != Pass || pass.Total != 0 || len(pass.Rows) != 0 {
		t.Fatalf("all offered = %+v, want pass with no rows", pass)
	}
}

func TestCompareExtensions_CapsRows(t *testing.T) {
	var installed []extension
	for i := 0; i < MaxRows+1; i++ {
		installed = append(installed, extension{fmt.Sprintf("ext%02d", i), "1.0"})
	}
	got := compareExtensions(installed, nil)
	if len(got.Rows) != MaxRows || got.Total != MaxRows+1 || got.Status != Block {
		t.Fatalf("51 missing = %d rows, total %d, %s; want 50, 51, block", len(got.Rows),
			got.Total, got.Status)
	}
}

func TestUnverifiedExtensions_IsUnknownNeverPass(t *testing.T) {
	got := unverifiedExtensions([]extension{{"pgcrypto", "1.3"}}, "no target_database")
	if got.Status != Unknown || got.Total != 1 || got.Rows[0]["extension"] != "pgcrypto" ||
		!strings.Contains(got.Reason, "no target_database") {
		t.Fatalf("unverified = %+v", got)
	}
}

func TestReplicationVerdict(t *testing.T) {
	in := func(src int, method string) input {
		return input{src: source{major: src}, target: Target{Major: 18, Method: method}}
	}
	cases := []struct {
		in           input
		slots, subs  int
		want         Status
		reasonSubstr string
	}{
		{in(16, PGUpgrade), 1, 0, Block, "does not carry logical slots"},
		{in(14, PGUpgrade), 0, 2, Warn, "table sync state"},
		{in(17, PGUpgrade), 3, 1, Pass, "at least 3 max_replication_slots"},
		{in(16, BlueGreen), 1, 0, Warn, "recreate the slots"},
		{in(17, Logical), 0, 1, Warn, "logical upgrade"},
	}
	for _, c := range cases {
		got, reason := replicationVerdict(c.in, c.slots, c.subs)
		if got != c.want || !strings.Contains(reason, c.reasonSubstr) {
			t.Fatalf("verdict(PG%d %s, %d slots, %d subs) = %s %q, want %s with %q",
				c.in.src.major, c.in.target.Method, c.slots, c.subs, got, reason, c.want,
				c.reasonSubstr)
		}
	}
}

func TestCheckSupport(t *testing.T) {
	run := func(provider string, src, target int, now string) Check {
		c, err := checkSupport(context.Background(), nil, input{src: source{major: src},
			target: Target{Major: target, Provider: provider}, now: at(now)})
		if err != nil {
			t.Fatalf("checkSupport: %v", err)
		}
		return c
	}
	cases := []struct {
		provider     string
		src, target  int
		now          string
		want         Status
		rows         int
		reasonSubstr string
	}{
		{"self-managed", 14, 18, "2026-11-12", Pass, 2, "until 2030-11-14"},
		{"rds", 14, 17, "2026-11-12", Pass, 2, "2030-02-28"},
		{"rds", 12, 13, "2026-11-12", Block, 2, "left rds standard support"},
		{"rds", 12, 13, "2026-02-28", Warn, 2, "within a year"}, // the last supported day
		{"rds", 12, 13, "2026-03-01", Block, 2, "2026-02-28"},   // the day after
		// a year and a day before (2028 is a leap year)
		{"aurora", 15, 16, "2028-02-28", Pass, 2, "2029-02-28"},
		{"cloud-sql", 14, 18, "2026-11-12", Unknown, 0, "no dated support table"},
		{"", 14, 18, "2026-11-12", Unknown, 0, `provider ""`},
	}
	for _, c := range cases {
		got := run(c.provider, c.src, c.target, c.now)
		if got.Status != c.want || len(got.Rows) != c.rows || got.Total != c.rows ||
			!strings.Contains(got.Reason, c.reasonSubstr) {
			t.Fatalf("support(%q %d->%d @%s) = %s %d rows %q, want %s %d rows with %q",
				c.provider, c.src, c.target, c.now, got.Status, len(got.Rows), got.Reason,
				c.want, c.rows, c.reasonSubstr)
		}
	}
	got := run("rds", 14, 18, "2026-11-12")
	if got.Rows[0]["side"] != "source" || got.Rows[0]["support_ends"] != "2027-02-28" ||
		got.Rows[1]["read"] == "" || !strings.HasPrefix(got.Rows[1]["source"], "https://") {
		t.Fatalf("support rows = %v, want dated source and target rows", got.Rows)
	}
}

func TestUnknownFrom_KeepsSQLState(t *testing.T) {
	c := check{id: "U5", title: "Collations"}
	got := unknownFrom(c, fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42501",
		Message: "permission denied for function pg_collation_actual_version"}))
	if got.Status != Unknown || got.SQLState != "42501" || got.ID != "U5" ||
		!strings.Contains(got.Reason, "permission denied") || got.Rows == nil {
		t.Fatalf("unknownFrom = %+v", got)
	}
	plain := unknownFrom(c, errors.New("conn closed"))
	if plain.SQLState != "" || plain.Status != Unknown {
		t.Fatalf("non-PG error = %+v, want unknown without a SQLSTATE", plain)
	}
}

func TestFindings_OnlyNonPassingChecks(t *testing.T) {
	rep := Report{SourceMajor: 14, Target: Target{Major: 18, Method: PGUpgrade,
		Provider: "rds"}, Checks: []Check{
		{ID: "U1", Title: "Identity", Status: Pass},
		{ID: "U3", Title: "Slots", Status: Block, Reason: "slots vanish", Total: 1,
			Rows: []Row{{"slot": "s1"}}},
		{ID: "U5", Title: "Collations", Status: Unknown, SQLState: "42501",
			Skipped: []string{"database: PG14"}},
		{ID: "U6", Title: "Settings", Status: Warn},
	}}
	got := Findings(rep)
	if len(got) != 3 {
		t.Fatalf("findings = %d, want 3 (pass is not a finding)", len(got))
	}
	u3 := got[0]
	if u3.Category != Category || u3.Severity != "warning" ||
		u3.ObjectIdentifier != "PostgreSQL 14 to 18 via pg_upgrade: U3" ||
		u3.Recommendation != "slots vanish" || u3.RecommendedSQL != "" ||
		u3.RollbackSQL != "" || u3.Detail["total"] != 1 || u3.Detail["status"] != "block" {
		t.Fatalf("U3 finding = %+v", u3)
	}
	if rows, _ := u3.Detail["rows"].([]any); len(rows) != 1 {
		t.Fatalf("U3 rows = %#v, want one", u3.Detail["rows"])
	}
	if got[1].Severity != "info" || got[1].Detail["sqlstate"] != "42501" ||
		got[1].Detail["skipped"] == nil {
		t.Fatalf("U5 finding = %+v", got[1])
	}
	if got[2].Severity != "info" || !strings.Contains(got[2].Title, "(warn)") {
		t.Fatalf("U6 finding = %+v", got[2])
	}
	if len(Findings(Report{})) != 0 {
		t.Fatal("an empty report has no findings")
	}
}

func TestReportCheck(t *testing.T) {
	r := Report{Checks: []Check{{ID: "U2", Status: Unknown}}}
	if c, ok := r.Check("U2"); !ok || c.Status != Unknown {
		t.Fatalf("Check(U2) = %+v %v", c, ok)
	}
	if _, ok := r.Check("U9"); ok {
		t.Fatal("Check(U9) found a check that does not exist")
	}
}
