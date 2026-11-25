package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/skipbudget"
)

// env-pg-a: the first PostgreSQL 15 run of the suite found these version-
// gated skips UNLISTED. Each gate is a real PostgreSQL feature boundary, so
// the allow-list must accept the skip exactly on the majors that lack the
// feature (14, 15 and, for PG17 features, 16) and nowhere else.

// No concurrent access tests: ParseRules and Check keep no shared state.

var allMajors = []int{14, 15, 16, 17, 18}

type gatedSkip struct {
	pkg, test, text string
	allowed         []int // majors on which the skip is allowed
}

// The skip texts are the tests' own t.Skip messages as the PG15 run
// printed them.
var pg15GatedSkips = []gatedSkip{
	{"internal/collector", "TestCapabilities_StalePG16VersionOnPG17",
		"capabilities_db_test.go:274: needs a PG17+ server, have 150019", []int{14, 15, 16}},
	// Its mirror runs on 14 to 16 and skips on 17 and 18; the PG17 host run
	// of the collector package found it UNLISTED too.
	{"internal/collector", "TestCapabilities_StalePG17VersionOnPG14",
		"capabilities_db_test.go:316: needs a pre-17 server, have 170010", []int{17, 18}},
	{"internal/collector", "TestCapabilities_IOGateFollowsLiveVersion",
		"capabilities_db_test.go:299: needs a PG16+ server (pg_stat_io), have 150019",
		[]int{14, 15}},
	{"internal/collector", "TestCollectSystem_MissingNumDoneFallsBack",
		"checkpoint_split_test.go:143: needs PG 17 (pg_stat_checkpointer without num_done)," +
			" server is 150019", []int{14, 15, 16, 18}},
	{"internal/collector", "TestCollect_FailedExtraIsUnavailableNotEmpty/io",
		"extras_unavailable_db_test.go:66: pg_stat_io is not read below PG16 (server 150019)",
		[]int{14, 15}},
	{"internal/executor", "TestActionPool_MaintainRoleVacuumsOn17",
		"action_pool_db_test.go:282: pg_maintain exists from PostgreSQL 17; before it only" +
			" the owner may VACUUM", []int{14, 15, 16}},
	{"internal/shadow", "TestDetectOwnTableActionsWalkTheTimeIndex",
		"scorer_detect_config_db_test.go:146: EXPLAIN (GENERIC_PLAN) requires PostgreSQL 16+" +
			" (server_version_num 150019)", []int{14, 15}},
	{"internal/shadow", "TestScorerReadsWalkTheirIndexes",
		"scorer_facts_plan_test.go:75: EXPLAIN (GENERIC_PLAN) requires PostgreSQL 16+" +
			" (server_version_num 150019)", []int{14, 15}},
}

// skipStream is the go test -json lines of one skipped test with its text.
func skipStream(t *testing.T, pkg, test, text string) string {
	t.Helper()
	var b strings.Builder
	full := "github.com/pg-sage/sidecar/" + pkg
	for _, e := range []map[string]string{
		{"Action": "run", "Package": full, "Test": test},
		{"Action": "output", "Package": full, "Test": test, "Output": "=== RUN   " + test + "\n"},
		{"Action": "output", "Package": full, "Test": test, "Output": "    " + text + "\n"},
		{"Action": "output", "Package": full, "Test": test, "Output": "--- SKIP: " + test + "\n"},
		{"Action": "skip", "Package": full, "Test": test},
	} {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func checkRepoAt(t *testing.T, rules []skipbudget.Rule, pg int, stream string) skipbudget.Report {
	t.Helper()
	report, err := skipbudget.Check(strings.NewReader(stream), rules, skipbudget.Options{PG: pg})
	if err != nil {
		t.Fatalf("Check at pg %d: %v", pg, err)
	}
	if report.Skipped != 1 {
		t.Fatalf("pg %d: %d skips read, want the one in the stream", pg, report.Skipped)
	}
	return report
}

func contains(majors []int, pg int) bool {
	for _, m := range majors {
		if m == pg {
			return true
		}
	}
	return false
}

// Boundary: each gate is checked at every major from 14 to 18, so a rule
// one major too wide (allowing the skip where the feature exists) or too
// narrow (failing the leg that lacks it) fails here.
func TestRepoAllowlistAllowsPG15GatedSkipsOnlyBelowTheirMajor(t *testing.T) {
	rules := repoRules(t)
	for _, c := range pg15GatedSkips {
		stream := skipStream(t, c.pkg, c.test, c.text)
		for _, pg := range allMajors {
			report := checkRepoAt(t, rules, pg, stream)
			want := contains(c.allowed, pg)
			if got := len(report.Allowed) == 1; got != want {
				t.Errorf("%s %s on PG%d: allowed = %v, want %v (violations %+v)",
					c.pkg, c.test, pg, got, want, report.Violations)
				continue
			}
			if !want && !strings.Contains(report.Violations[0].Mismatch, "PostgreSQL") {
				t.Errorf("%s on PG%d: mismatch %q must name the major the rule skips",
					c.test, pg, report.Violations[0].Mismatch)
			}
		}
	}
}

// Invalid input: the rules carry reason= qualifiers, so the same test
// skipping for another reason on an old major is still UNLISTED. The io
// rule must not cover the other extras of the same test.
func TestRepoAllowlistPG15GatedRulesNeedTheirOwnReason(t *testing.T) {
	rules := repoRules(t)
	cases := []struct{ pkg, test, text string }{
		{"internal/collector", "TestCollect_FailedExtraIsUnavailableNotEmpty/prepared_xacts",
			"extras_unavailable_db_test.go:70: cannot connect to PostgreSQL"},
		{"internal/collector", "TestCapabilities_StalePG16VersionOnPG17",
			"collector_integ_test.go:27: cannot connect to PostgreSQL: refused"},
		{"internal/executor", "TestActionPool_MaintainRoleVacuumsOn17",
			"action_pool_db_test.go:20: SAGE_TEST_DATABASE_URL not set"},
		{"internal/shadow", "TestScorerReadsWalkTheirIndexes", ""},
	}
	for _, c := range cases {
		report := checkRepoAt(t, rules, 15, skipStream(t, c.pkg, c.test, c.text))
		if len(report.Allowed) != 0 || len(report.Violations) != 1 {
			t.Errorf("%s %q on PG15: allowed %d, want UNLISTED", c.test, c.text,
				len(report.Allowed))
			continue
		}
		if !strings.Contains(report.Violations[0].Mismatch, "reason=") {
			t.Errorf("%s: mismatch %q must name the reason qualifier", c.test,
				report.Violations[0].Mismatch)
		}
	}
}

// image-limited skips (the extension is absent from postgres:15, not from
// CI's image) must never be allowed: CI's PG15 leg has HypoPG, pg_hint_plan
// and pgvector, and there these tests run.
func TestRepoAllowlistNeverAllowsImageLimitedPG15Skips(t *testing.T) {
	rules := repoRules(t)
	cases := []struct{ pkg, test, text string }{
		{"internal/optimizer", "TestFloor_HypoPGAdmitsTheCustomerIndex",
			"floor_db_test.go:68: HypoPG server extension unavailable on this image"},
		{"internal/tuner", "TestDetectHintPlan_DB_PreQueryIDLayoutIsNotReady",
			"detector_db_test.go:201: needs the pre-1.7 hint table" +
				" (pg_hint_plan 1.4-1.6, PG14-16)"},
		{"internal/optimizer", "TestCheckDuplicate_VectorAndVarcharOnRealPostgres",
			"validate_db_test.go:59: pgvector server extension unavailable"},
		// Not TestRealPGAuditTaggedLongAction: CI's image installs pgaudit but
		// does not preload it or write jsonlog, so it skips on every leg; like
		// TestRealPGAuditCorrelation it runs, required, in ci.yml agent-servers.
	}
	for _, c := range cases {
		for _, pg := range []int{14, 15} {
			report := checkRepoAt(t, rules, pg, skipStream(t, c.pkg, c.test, c.text))
			if len(report.Allowed) != 0 {
				t.Errorf("%s on PG%d is allowed by line %d; an image-limited skip must stay"+
					" UNLISTED", c.test, pg, report.Allowed[0].Rule.Line)
			}
		}
	}
}

// The PG15 run reported the optimizer GENERIC_PLAN rule STALE: its test was
// removed with the unused plan capture (4f9f795b), so the rule allowed a
// skip no test can produce.
func TestRepoAllowlistHasNoRuleForTheRemovedPlanCaptureTest(t *testing.T) {
	for _, rule := range repoRules(t) {
		if rule.Package.MatchString("internal/optimizer") &&
			rule.Test.MatchString("TestGenericPlanCapturesUnboundParameters") {
			t.Errorf("line %d still allows the removed TestGenericPlanCapturesUnboundParameters",
				rule.Line)
		}
	}
}

// Nil/empty: a stream with no skips reports none of the new rules as
// allowing anything, and on PG17 the PG16-and-older gates are not stale
// (they do not apply there).
func TestRepoAllowlistPG15GatedRulesWithNoSkips(t *testing.T) {
	rules := repoRules(t)
	report, err := skipbudget.Check(strings.NewReader(""), rules, skipbudget.Options{PG: 17})
	if err != nil {
		t.Fatalf("Check of an empty stream: %v", err)
	}
	if report.Skipped != 0 || len(report.Allowed) != 0 || len(report.Violations) != 0 {
		t.Fatalf("empty stream: report = %+v, want nothing", report)
	}
	for _, rule := range report.Stale {
		for _, c := range pg15GatedSkips {
			name, _, _ := strings.Cut(c.test, "/")
			if rule.Test.MatchString(name) && !contains(c.allowed, 17) {
				t.Errorf("line %d (%s) is stale on PG17; it must not apply there", rule.Line,
					rule.Text)
			}
		}
	}
}
