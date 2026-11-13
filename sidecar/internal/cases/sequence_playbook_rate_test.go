package cases

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// playbooks-03b: Step 1 of the sequence forward-fix script states the
// runway rate pg_sage measured (copied by the cases API from the open
// forecast_sequence_runway finding), or says it is not measured yet.
// No concurrent access tests: the script builder is a pure function of
// its finding.

var runwayReadAt = time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)

// rateFinding is a sequence finding carrying the runway values the cases
// API attaches.
func rateFinding(rate, seconds, readAt any) SourceFinding {
	f := sequenceFinding("public.orders_id_seq")
	f.Detail[DetailRunwayRatePerS] = rate
	f.Detail[DetailRunwaySecondsToLimit] = seconds
	f.Detail[DetailRunwayReadAt] = readAt
	return f
}

func rateScript(t *testing.T, f SourceFinding) *ScriptOutput {
	t.Helper()
	got := projectFinding(f)
	assertActionTypes(t, f.ObjectIdentifier, got, "prepare_sequence_capacity_migration")
	if got.ActionCandidates[0].ScriptOutput == nil {
		t.Fatalf("%s: no script output", f.ObjectIdentifier)
	}
	return got.ActionCandidates[0].ScriptOutput
}

const notMeasured = "-- Rate: not measured yet (no runway forecast for this sequence)."

// Happy path: the measured rate and the time to the limit, with the
// forecast's read time, sit in Step 1 before the runway query.
func TestSequenceRateStatedInStepOne(t *testing.T) {
	script := rateScript(t, rateFinding(12.5, 864000.0, runwayReadAt))
	sql := script.MigrationSQL
	rate := "-- Measured rate: rises 12.5 values/s (runway forecast read " +
		"2026-09-20T08:00:00Z)."
	when := "-- At that rate the limit is reached in about 10 days (2026-09-30)."
	for _, want := range []string{rate, when} {
		if !strings.Contains(sql, want+"\n") {
			t.Fatalf("script lacks line %q:\n%s", want, sql)
		}
	}
	step1 := strings.Index(sql, "-- Step 1 (read-only)")
	at := strings.Index(sql, rate)
	runway := strings.Index(sql, "FROM pg_catalog.pg_sequences s")
	if !(step1 >= 0 && step1 < at && at < runway) {
		t.Fatalf("rate line not in step 1 before the runway query (%d, %d, %d)",
			step1, at, runway)
	}
	if strings.Contains(sql, "not measured yet") {
		t.Fatalf("measured script also says not measured:\n%s", sql)
	}
	for _, want := range []string{"Measured rate: rises 12.5 values/s (runway forecast " +
		"read 2026-09-20T08:00:00Z).", "about 10 days (2026-09-30)."} {
		if !strings.Contains(script.PRBody, want) {
			t.Fatalf("PR body lacks %q: %s", want, script.PRBody)
		}
	}
}

// Happy path: a descending sequence falls, at the absolute rate.
func TestSequenceRateDescendingSaysFalls(t *testing.T) {
	for _, rate := range []float64{12.5, -12.5} {
		f := rateFinding(rate, 864000.0, runwayReadAt)
		f.Detail["increment"] = -1.0
		sql := rateScript(t, f).MigrationSQL
		if !strings.Contains(sql, "-- Measured rate: falls 12.5 values/s") ||
			strings.Contains(sql, "rises") {
			t.Fatalf("rate %v: descending wording missing:\n%s", rate, sql)
		}
	}
}

// Invalid input: anything that is not a finite number, a positive time to
// the limit and a read time gives the not-measured comment, and no part
// of the value reaches the script.
func TestSequenceRateInvalidValuesSayNotMeasured(t *testing.T) {
	baseline := rateScript(t, sequenceFinding("public.orders_id_seq"))
	cases := map[string]SourceFinding{
		"rate string":       rateFinding("12; DROP TABLE x", 864000.0, runwayReadAt),
		"rate numeric text": rateFinding("12.5", 864000.0, runwayReadAt),
		"rate NaN":          rateFinding(math.NaN(), 864000.0, runwayReadAt),
		"rate Inf":          rateFinding(math.Inf(1), 864000.0, runwayReadAt),
		"rate nil":          rateFinding(nil, 864000.0, runwayReadAt),
		"rate map":          rateFinding(map[string]any{"v": 1.0}, 864000.0, runwayReadAt),
		"rate falls, asc":   rateFinding(-12.5, 864000.0, runwayReadAt),
		"seconds negative":  rateFinding(12.5, -1.0, runwayReadAt),
		"seconds zero":      rateFinding(12.5, 0.0, runwayReadAt),
		"seconds NaN":       rateFinding(12.5, math.NaN(), runwayReadAt),
		"seconds -Inf":      rateFinding(12.5, math.Inf(-1), runwayReadAt),
		"seconds +Inf":      rateFinding(12.5, math.Inf(1), runwayReadAt),
		"seconds string":    rateFinding(12.5, "864000; DROP TABLE x", runwayReadAt),
		"read_at missing":   rateFinding(12.5, 864000.0, nil),
		"read_at zero":      rateFinding(12.5, 864000.0, time.Time{}),
		"read_at text":      rateFinding(12.5, 864000.0, "yesterday; DROP TABLE x"),
	}
	for name, f := range cases {
		script := rateScript(t, f)
		if script.MigrationSQL != baseline.MigrationSQL || script.PRBody != baseline.PRBody {
			t.Fatalf("%s: script differs from the not-measured one:\n%s", name,
				script.MigrationSQL)
		}
		if strings.Contains(script.MigrationSQL+script.PRBody, "DROP TABLE x") {
			t.Fatalf("%s: detail text reached the script", name)
		}
	}
	if !strings.Contains(baseline.MigrationSQL, notMeasured+"\n") ||
		!strings.Contains(baseline.MigrationSQL, "compare remaining_values.") {
		t.Fatalf("not-measured comment missing:\n%s", baseline.MigrationSQL)
	}
}

// Nil/empty: no runway values, or no detail at all, says not measured;
// an empty identifier keeps today's blocked candidate without a script.
func TestSequenceRateMissingSaysNotMeasured(t *testing.T) {
	f := sequenceFinding("public.orders_id_seq")
	f.Detail = nil
	script := rateScript(t, f)
	if !strings.Contains(script.MigrationSQL, notMeasured) ||
		!strings.Contains(script.PRBody, "Rate: not measured yet (no runway forecast for "+
			"this sequence). Run step 1 again later and compare remaining_values.") {
		t.Fatalf("nil detail: not-measured text missing:\n%s\n%s", script.MigrationSQL,
			script.PRBody)
	}
	blank := rateFinding(12.5, 864000.0, runwayReadAt)
	blank.ObjectIdentifier = " "
	c := projectFinding(blank).ActionCandidates[0]
	if c.ScriptOutput != nil || c.BlockedReason != sequenceNoIdentityReason {
		t.Fatalf("blank identity: script %v blocked %q", c.ScriptOutput, c.BlockedReason)
	}
}

// Boundary: under one day, exactly one day, rounding, and a horizon too
// far to date.
func TestSequenceRateDayBoundaries(t *testing.T) {
	for _, tc := range []struct {
		seconds float64
		want    string
	}{
		{1, "reached in less than a day (2026-09-20)."},
		{86399, "reached in less than a day (2026-09-21)."},
		{86400, "reached in about 1 day (2026-09-21)."},
		{129599, "reached in about 1 day (2026-09-21)."},
		{129600, "reached in about 2 days (2026-09-21)."},
		{864000, "reached in about 10 days (2026-09-30)."},
		{36500 * 86400, "reached in about 36500 days (2126-08-27)."},
		{36501 * 86400, "-- At that rate the limit is more than 100 years away."},
		{1e300, "-- At that rate the limit is more than 100 years away."},
	} {
		sql := rateScript(t, rateFinding(12.5, tc.seconds, runwayReadAt)).MigrationSQL
		if !strings.Contains(sql, tc.want+"\n") {
			t.Fatalf("seconds %v: want %q in:\n%s", tc.seconds, tc.want, sql)
		}
	}
}

// Boundary: the runway stores its rate rounded to 0.01; a slower sequence
// is stored as 0 while its time to the limit is still measured.
func TestSequenceRateBelowStoredPrecision(t *testing.T) {
	sql := rateScript(t, rateFinding(0.0, 864000.0, runwayReadAt)).MigrationSQL
	if !strings.Contains(sql, "-- Measured rate: rises under 0.01 values/s") ||
		!strings.Contains(sql, "about 10 days") {
		t.Fatalf("slow rate wording missing:\n%s", sql)
	}
}

// Input forms: the values as they come back from JSON (json.Number, an
// RFC 3339 string) and as integers.
func TestSequenceRateAcceptsDecodedForms(t *testing.T) {
	for name, f := range map[string]SourceFinding{
		"json.Number": rateFinding(json.Number("12.5"), json.Number("864000"),
			"2026-09-20T08:00:00Z"),
		"ints":       rateFinding(int64(12), 864000, runwayReadAt),
		"other zone": rateFinding(12.5, 864000.0, runwayReadAt.In(time.FixedZone("x", 3600))),
	} {
		sql := rateScript(t, f).MigrationSQL
		if !strings.Contains(sql, "(runway forecast read 2026-09-20T08:00:00Z).") ||
			!strings.Contains(sql, "about 10 days (2026-09-30).") {
			t.Fatalf("%s: rate not stated:\n%s", name, sql)
		}
	}
}

// State transition: the review-only contract and the three steps' SQL do
// not change when the rate is stated; only the rate lines do.
func TestSequenceRateKeepsContractAndSteps(t *testing.T) {
	plain := projectFinding(sequenceFinding("public.orders_id_seq")).ActionCandidates[0]
	rated := projectFinding(rateFinding(12.5, 864000.0, runwayReadAt)).ActionCandidates[0]
	if rated.RiskTier != "high" || rated.RollbackClass != "forward_fix_only" ||
		!reflect.DeepEqual(rated.OutputModes, []string{"generate_pr_or_script"}) ||
		rated.ExpiresAt == nil || !rated.ExpiresAt.Equal(evidenceAnchor.Add(24*time.Hour)) {
		t.Fatalf("contract changed: %+v", rated)
	}
	if !reflect.DeepEqual(plain.ScriptOutput.VerificationSQL,
		rated.ScriptOutput.VerificationSQL) ||
		plain.ScriptOutput.Filename != rated.ScriptOutput.Filename {
		t.Fatalf("verification or filename changed")
	}
	strip := func(sql string) string {
		var keep []string
		for _, l := range strings.Split(sql, "\n") {
			if !strings.HasPrefix(l, "-- Rate: ") && !strings.HasPrefix(l, "-- Measured") &&
				!strings.HasPrefix(l, "-- At that rate") &&
				l != "-- Run step 1 again later and compare remaining_values." {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	if strip(plain.ScriptOutput.MigrationSQL) != strip(rated.ScriptOutput.MigrationSQL) {
		t.Fatalf("steps changed beyond the rate lines:\n%s", rated.ScriptOutput.MigrationSQL)
	}
}

// rateLine is the only shape a line added by the rate may take.
var rateLine = regexp.MustCompile(`^-- (Measured rate: (rises|falls) (under 0\.01|` +
	`[0-9]+(\.[0-9]+)?) values/s \(runway forecast read ` +
	`[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z\)\.|At that rate the limit is ` +
	`(reached in (less than a day|about [0-9]+ days?) \([0-9]{4,}-[0-9]{2}-[0-9]{2}\)|` +
	`more than 100 years away)\.)$`)

var rateSeeds = []string{
	"\nDROP TABLE x", "*/ DROP TABLE x; /*", "12\n;DROP TABLE x", "12.5\r\nDROP TABLE x",
	"2026-09-20T08:00:00Z\nDROP TABLE x", "-- DROP TABLE x", "1e3*/DROP TABLE x",
	"864000", "12.5", "NaN", "Inf", "",
}

// checkRateLines fails when a line the rate added is not a rate line, or
// when seed text reaches the script.
func checkRateLines(t *testing.T, base map[string]bool, f SourceFinding, seed string) {
	t.Helper()
	script := rateScript(t, f)
	for _, l := range strings.Split(script.MigrationSQL, "\n") {
		if !base[l] && !rateLine.MatchString(l) {
			t.Fatalf("seed %q: unexpected script line %q", seed, l)
		}
	}
	if strings.Contains(script.MigrationSQL+script.PRBody, "DROP TABLE x") {
		t.Fatalf("seed %q reached the script", seed)
	}
}

func baseLines(t *testing.T) map[string]bool {
	t.Helper()
	base := map[string]bool{}
	for _, l := range strings.Split(rateScript(t,
		sequenceFinding("public.orders_id_seq")).MigrationSQL, "\n") {
		base[l] = true
	}
	return base
}

// CHECK-02: no runway detail text, in any key or form, reaches a comment
// line; only the fixed rate lines are ever added.
func TestSequenceScriptNeverPutsDetailTextOnACommentLine(t *testing.T) {
	base := baseLines(t)
	keys := []string{DetailRunwayRatePerS, DetailRunwaySecondsToLimit, DetailRunwayReadAt,
		"increment", "data_type"}
	for _, seed := range rateSeeds {
		for _, key := range keys {
			f := rateFinding(12.5, 864000.0, runwayReadAt)
			f.Detail[key] = seed
			checkRateLines(t, base, f, seed)
			var raw any = json.RawMessage(`"` + seed + `"`)
			f.Detail[key] = raw
			checkRateLines(t, base, f, seed)
		}
	}
}

func FuzzSequenceRateDetail(f *testing.F) {
	for _, seed := range rateSeeds {
		f.Add(seed, 12.5, 864000.0)
	}
	f.Add("x", math.NaN(), math.Inf(1))
	f.Add("x", -3.0, 1e300)
	f.Fuzz(func(t *testing.T, text string, rate, seconds float64) {
		base := baseLines(t)
		finding := rateFinding(rate, seconds, runwayReadAt)
		finding.Detail["increment"] = text
		checkRateLines(t, base, finding, text)
		finding = rateFinding(text, seconds, text)
		checkRateLines(t, base, finding, text)
	})
}
