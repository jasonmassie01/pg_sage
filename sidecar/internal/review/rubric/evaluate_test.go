package rubric

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// review-01a: each source's result. A hidden fact is unknown or
// not_applicable, never pass. No concurrent access tests: Evaluate is pure.

var evalNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func one(t *testing.T, c CheckSpec, in Inputs) CheckResult {
	t.Helper()
	r := Rubric{Version: "t/1", Dimensions: []Dimension{{ID: "x", Title: "X",
		Checks: []CheckSpec{c}}}}
	out := Evaluate(r, in)
	if len(out) != 1 || out[0].Check != c.ID || out[0].Weight != c.Weight {
		t.Fatalf("Evaluate = %+v, want one result for %s", out, c.ID)
	}
	return out[0]
}

func report() firstlook.Report {
	return firstlook.Report{ID: 42, Checks: []firstlook.Check{
		{Rule: firstlook.RuleDuplicateIndex, Status: firstlook.CheckFinding},
		{Rule: firstlook.RuleRedundantIndex, Status: firstlook.CheckOK},
		{Rule: firstlook.RuleXIDRunway, Status: firstlook.CheckDegraded, Note: "timeout"},
		{Rule: "AP-03", Section: firstlook.SectionAgentPosture, Status: firstlook.CheckFinding},
		{Rule: "AP-08", Section: firstlook.SectionAgentPosture, Status: firstlook.CheckOK}},
		Items: []firstlook.Item{
			{Rule: firstlook.RuleDuplicateIndex, Severity: firstlook.SeverityInfo},
			{Rule: "AP-03", Section: firstlook.SectionAgentPosture,
				Severity: firstlook.SeverityCritical}}}
}

func TestEvaluate_FirstLookAndPostureStatuses(t *testing.T) {
	in := Inputs{Now: evalNow, FirstLook: FirstLookInput{Found: true, Report: report()}}
	cases := []struct {
		src  Source
		ref  string
		want Status
		why  string
	}{
		{SourceFirstLook, firstlook.RuleDuplicateIndex, StatusWarn, ""},
		{SourceFirstLook, firstlook.RuleRedundantIndex, StatusPass, ""},
		{SourceFirstLook, firstlook.RuleXIDRunway, StatusUnknown, "degraded"},
		{SourceFirstLook, firstlook.RuleUnindexedFK, StatusUnknown, "did not run"},
		{SourcePosture, "AP-03", StatusFail, ""},
		{SourcePosture, "AP-08", StatusPass, ""},
		{SourcePosture, "AP-15", StatusUnknown, "did not run"},
	}
	for _, tc := range cases {
		got := one(t, CheckSpec{ID: "x." + tc.ref, Source: tc.src, Ref: tc.ref, Weight: 2}, in)
		if got.Status != tc.want || !strings.Contains(got.Reason, tc.why) {
			t.Errorf("%s = %s %q, want %s %q", tc.ref, got.Status, got.Reason, tc.want, tc.why)
		}
		if got.Status != StatusUnknown && (len(got.Evidence) != 1 ||
			got.Evidence[0] != "first_look:42#"+tc.ref) {
			t.Errorf("%s evidence = %v, want the first look item", tc.ref, got.Evidence)
		}
	}
}

func TestEvaluate_FirstLookUnreadableOrMissingIsUnknownNamingFirstlook(t *testing.T) {
	c := CheckSpec{ID: "x.dup", Source: SourceFirstLook, Ref: firstlook.RuleDuplicateIndex,
		Weight: 1}
	got := one(t, c, Inputs{FirstLook: FirstLookInput{Err: errors.New("connection reset")}})
	if got.Status != StatusUnknown || !strings.Contains(got.Reason, "firstlook") ||
		!strings.Contains(got.Reason, "connection reset") {
		t.Fatalf("read error = %+v, want unknown naming firstlook and the error", got)
	}
	got = one(t, c, Inputs{})
	if got.Status != StatusUnknown || !strings.Contains(got.Reason, "no first look") {
		t.Fatalf("no report = %+v, want unknown", got)
	}
}

func TestEvaluate_FindingsNeedTheirProducerToHaveRun(t *testing.T) {
	c := CheckSpec{ID: "x.ts", Source: SourceFinding, Ref: "temp_spill", Weight: 1}
	open := []OpenFinding{{ID: 7, Category: "temp_spill", Severity: "warning"},
		{ID: 9, Category: "temp_spill", Severity: "info"}}
	got := one(t, c, Inputs{Findings: FindingsInput{Open: open}})
	if got.Status != StatusFail || strings.Join(got.Evidence, ",") != "finding:7,finding:9" {
		t.Fatalf("open warning = %+v, want fail citing both findings", got)
	}
	got = one(t, c, Inputs{Findings: FindingsInput{}})
	if got.Status != StatusUnknown || !strings.Contains(got.Reason, "analyzer") {
		t.Fatalf("no producer run = %+v, want unknown naming the analyzer", got)
	}
	ran := FindingsInput{Ran: map[string]bool{"analyzer": true}}
	if got = one(t, c, Inputs{Findings: ran}); got.Status != StatusPass {
		t.Fatalf("analyzer ran, nothing open = %+v, want pass", got)
	}
	info := FindingsInput{Ran: ran.Ran, Open: open[1:]}
	if got = one(t, c, Inputs{Findings: info}); got.Status != StatusWarn {
		t.Fatalf("info only = %+v, want warn", got)
	}
	bad := FindingsInput{Err: errors.New("permission denied for table findings")}
	if got = one(t, c, Inputs{Findings: bad}); got.Status != StatusUnknown ||
		!strings.Contains(got.Reason, "permission denied") {
		t.Fatalf("read error = %+v, want unknown with the error", got)
	}
}

func TestEvaluate_PlatformAloneDecidesNotApplicable(t *testing.T) {
	c := CheckSpec{ID: "x.arch", Source: SourceFinding, Ref: "wal_archive_failing",
		Weight: 3, Platforms: []string{"postgres", "cnpg"}}
	f := FindingsInput{Ran: map[string]bool{"analyzer": true}}
	if got := one(t, c, Inputs{Provider: "rds", Findings: f}); got.Status !=
		StatusNotApplicable || !strings.Contains(got.Reason, "rds") {
		t.Fatalf("on rds = %+v, want not_applicable naming the platform", got)
	}
	for _, p := range []string{"postgres", "", "unknown"} {
		if got := one(t, c, Inputs{Provider: p, Findings: f}); got.Status != StatusPass {
			t.Errorf("on %q = %+v, want graded", p, got)
		}
	}
}

func TestEvaluate_RightSizingIsUnknownWithoutCostRates(t *testing.T) {
	c := CheckSpec{ID: "x.rs", Source: SourceFinding, Ref: "instance_rightsize", Weight: 1,
		Requires: RequiresCostRates}
	f := FindingsInput{Ran: map[string]bool{"cost": true, "analyzer": true}}
	got := one(t, c, Inputs{Findings: f})
	if got.Status != StatusUnknown || !strings.Contains(got.Reason, "cost.rates") {
		t.Fatalf("no rates = %+v, want unknown naming cost.rates", got)
	}
	if got = one(t, c, Inputs{Findings: f, CostRates: true}); got.Status != StatusPass {
		t.Fatalf("rates set = %+v, want pass", got)
	}
}

func TestEvaluate_LintScan(t *testing.T) {
	in := Inputs{Lint: LintInput{Ran: true,
		Findings: map[string]string{"lint_int_pk": "warning", "lint_timestamp_no_tz": "info"},
		Failed:   map[string]bool{"lint_fk_type_mismatch": true},
		Disabled: map[string]bool{"lint_no_primary_key": true}}}
	cases := map[string]Status{"lint_int_pk": StatusFail, "lint_timestamp_no_tz": StatusWarn,
		"lint_fk_type_mismatch": StatusUnknown, "lint_no_primary_key": StatusUnknown,
		"lint_serial_usage": StatusPass}
	for ref, want := range cases {
		got := one(t, CheckSpec{ID: "x." + ref, Source: SourceLint, Ref: ref, Weight: 1}, in)
		if got.Status != want {
			t.Errorf("%s = %+v, want %s", ref, got, want)
		}
	}
	got := one(t, CheckSpec{ID: "x.l", Source: SourceLint, Ref: "lint_int_pk", Weight: 1},
		Inputs{Lint: LintInput{Err: errors.New("lint scan: canceled")}})
	if got.Status != StatusUnknown || !strings.Contains(got.Reason, "schema_lint") {
		t.Fatalf("scan error = %+v, want unknown naming schema_lint", got)
	}
}

func TestEvaluate_RestoreDrill(t *testing.T) {
	c := CheckSpec{ID: "x.drill", Source: SourceDrill, Ref: "last_passed", Weight: 5}
	cases := []struct {
		name string
		in   DrillInput
		want Status
	}{
		{"never drilled", DrillInput{}, StatusUnknown},
		{"never passed", DrillInput{Attempted: true}, StatusFail},
		{"recent pass", DrillInput{Attempted: true, LastAttemptPassed: true, LastPassedID: 3,
			LastPassedAt: evalNow.Add(-24 * time.Hour)}, StatusPass},
		{"35 days old", DrillInput{Attempted: true, LastAttemptPassed: true, LastPassedID: 3,
			LastPassedAt: evalNow.Add(-36 * 24 * time.Hour)}, StatusWarn},
		{"failed after an old pass", DrillInput{Attempted: true, LastPassedID: 3,
			LastPassedAt: evalNow.Add(-40 * 24 * time.Hour)}, StatusFail},
		{"unreadable", DrillInput{Err: errors.New("no store")}, StatusUnknown},
	}
	for _, tc := range cases {
		got := one(t, c, Inputs{Now: evalNow, Drill: tc.in})
		if got.Status != tc.want {
			t.Errorf("%s = %+v, want %s", tc.name, got, tc.want)
		}
		if tc.want == StatusPass && (len(got.Evidence) != 1 || got.Evidence[0] != "drill:3") {
			t.Errorf("%s evidence = %v, want drill:3", tc.name, got.Evidence)
		}
	}
}

func TestEvaluate_SettingsAndUpgrade(t *testing.T) {
	eol := CheckSpec{ID: "x.eol", Source: SourceSetting, Ref: "server_version_eol", Weight: 3}
	pr := CheckSpec{ID: "x.pr", Source: SourceSetting, Ref: "pending_restart", Weight: 1}
	up := CheckSpec{ID: "x.up", Source: SourceUpgrade, Ref: "readiness", Weight: 1}
	ran := FindingsInput{Ran: map[string]bool{"upgradecheck": true}}
	at := func(v int, pending ...string) Inputs {
		return Inputs{Now: evalNow, Findings: ran,
			Settings: SettingsInput{ServerVersionNum: v, PendingRestart: pending}}
	}
	checks := []struct {
		c    CheckSpec
		in   Inputs
		want Status
	}{
		{eol, at(170004), StatusPass}, {eol, at(120020), StatusFail},
		{eol, at(150010), StatusPass}, {eol, at(990000), StatusUnknown},
		{eol, Inputs{}, StatusUnknown},
		{pr, at(170004), StatusPass}, {pr, at(170004, "shared_buffers"), StatusWarn},
		{up, at(170004), StatusNotApplicable}, {up, at(130010), StatusPass},
		{up, Inputs{Now: evalNow, Settings: SettingsInput{ServerVersionNum: 130010}},
			StatusUnknown},
	}
	for i, tc := range checks {
		if got := one(t, tc.c, tc.in); got.Status != tc.want {
			t.Errorf("case %d %s = %+v, want %s", i, tc.c.Ref, got, tc.want)
		}
	}
	got := one(t, pr, Inputs{Settings: SettingsInput{Err: errors.New("statement timeout")}})
	if got.Status != StatusUnknown || !strings.Contains(got.Reason, "statement timeout") {
		t.Fatalf("probe error = %+v, want unknown with the error", got)
	}
}
