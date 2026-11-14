package analyzer

import (
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// No concurrent access tests: the rule functions are pure over one cycle's
// reading, which only the analyzer's cycle goroutine builds and reads.

func cronExtras(r *cronReading) *RuleExtras {
	return &RuleExtras{ext: &extReading{cron: r}}
}

func TestExtCronRules_NilReadingEmitsNothing(t *testing.T) {
	for _, ex := range []*RuleExtras{nil, {}, {ext: &extReading{}}} {
		if got := rulePgCronJobFailed(nil, nil, nil, ex); got != nil {
			t.Errorf("job failed on %+v = %v, want nil", ex, got)
		}
		if got := rulePgCronHistoryUnbounded(nil, nil, nil, ex); got != nil {
			t.Errorf("history on %+v = %v, want nil", ex, got)
		}
	}
}

func TestExtCronJobFailed_OneFindingPerJob(t *testing.T) {
	name := "nightly-vacuum"
	at := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	r := &cronReading{Schema: "cron", failuresRead: true, Failures: []cronFailure{
		{JobID: 7, JobName: &name, Failures: 3, LastFailedAt: at, LastMessage: "boom"},
		{JobID: 9, Failures: 1, LastFailedAt: at.Add(-time.Hour), LastMessage: "x"},
	}}
	got := rulePgCronJobFailed(nil, nil, nil, cronExtras(r))
	if len(got) != 2 {
		t.Fatalf("findings = %d, want 2: %+v", len(got), got)
	}
	f := got[0]
	if f.Category != categoryCronJobFailed || f.Severity != "warning" ||
		f.ObjectIdentifier != "pg_cron:job:7" || f.RecommendedSQL != "" {
		t.Fatalf("finding = %+v", f)
	}
	if f.Detail["jobid"] != int64(7) || f.Detail["failures"] != int64(3) ||
		f.Detail["jobname"] != name || f.Detail["last_message"] != "boom" ||
		f.Detail["last_failed_at"] != at.Format(time.RFC3339) ||
		f.Detail["visibility"] != "all_jobs" {
		t.Fatalf("detail = %+v", f.Detail)
	}
	if !strings.Contains(f.Title, name) || !strings.Contains(f.Recommendation, "cron.") {
		t.Fatalf("title %q / recommendation %q", f.Title, f.Recommendation)
	}
	if _, ok := got[1].Detail["jobname"]; ok {
		t.Fatalf("a job without a name has detail.jobname: %+v", got[1].Detail)
	}
	if got[1].ObjectIdentifier != "pg_cron:job:9" || !strings.Contains(got[1].Title, "9") {
		t.Fatalf("second finding = %+v", got[1])
	}
}

func TestExtCronJobFailed_NotReadEmitsNothing(t *testing.T) {
	r := &cronReading{Schema: "cron", Failures: []cronFailure{{JobID: 1, Failures: 2}}}
	if got := rulePgCronJobFailed(nil, nil, nil, cronExtras(r)); len(got) != 0 {
		t.Fatalf("an unread failure list emitted %v", got)
	}
}

func TestExtCronJobFailed_OwnJobsOnlyReportsVisibility(t *testing.T) {
	r := &cronReading{Schema: "cron", failuresRead: true, OwnJobsOnly: true}
	got := rulePgCronJobFailed(nil, nil, nil, cronExtras(r))
	if len(got) != 1 {
		t.Fatalf("findings = %v, want one visibility finding", got)
	}
	f := got[0]
	if f.Severity != "info" || f.ObjectIdentifier != "pg_cron:visibility" ||
		f.Detail["visibility"] != "own_jobs_only" {
		t.Fatalf("visibility finding = %+v", f)
	}
	if !strings.Contains(f.Recommendation, "job_run_details") {
		t.Fatalf("recommendation %q does not name the table", f.Recommendation)
	}
	// Visible failures under row security carry the scope instead.
	r.Failures = []cronFailure{{JobID: 3, Failures: 1}}
	got = rulePgCronJobFailed(nil, nil, nil, cronExtras(r))
	if len(got) != 1 || got[0].ObjectIdentifier != "pg_cron:job:3" ||
		got[0].Detail["visibility"] != "own_jobs_only" {
		t.Fatalf("own-jobs failure = %+v", got)
	}
}

func TestExtCronMessage_RedactedAndClipped(t *testing.T) {
	msg := cronMessage("ERROR: card '4111-1111-1111-1111' declined")
	if strings.Contains(msg, "4111") || !strings.Contains(msg, "declined") {
		t.Fatalf("redacted message = %q", msg)
	}
	exact := strings.Repeat("é", cronMessageRunes)
	if got := cronMessage(exact); got != exact {
		t.Fatalf("a %d-rune message was changed: %d runes", cronMessageRunes,
			utf8.RuneCountInString(got))
	}
	long := cronMessage(exact + "z")
	if !utf8.ValidString(long) || utf8.RuneCountInString(long) > cronMessageRunes+1 ||
		!strings.HasSuffix(long, "…") {
		t.Fatalf("clipped message = %d runes, valid %v", utf8.RuneCountInString(long),
			utf8.ValidString(long))
	}
	if got := cronMessage(""); got != "" {
		t.Fatalf("empty message = %q", got)
	}
}

func TestExtCronHistory_ReltuplesBoundary(t *testing.T) {
	cases := []struct {
		rows float64
		want int
	}{{-1, 0}, {0, 0}, {cronHistoryMaxRows, 0}, {cronHistoryMaxRows + 1, 1}}
	for _, c := range cases {
		r := &cronReading{Schema: `My "cron"`, historyRead: true, HistoryRows: c.rows}
		got := rulePgCronHistoryUnbounded(nil, nil, nil, cronExtras(r))
		if len(got) != c.want {
			t.Fatalf("reltuples %.0f: findings = %d, want %d", c.rows, len(got), c.want)
		}
		if c.want == 0 {
			continue
		}
		f := got[0]
		if f.Severity != "info" || f.ObjectIdentifier != `"My ""cron""".job_run_details` ||
			f.Detail["estimated_rows"] != int64(c.rows) || f.RecommendedSQL != "" ||
			!strings.Contains(f.Recommendation, "cron.schedule") {
			t.Fatalf("history finding = %+v", f)
		}
	}
	unread := &cronReading{Schema: "cron", HistoryRows: 5e6}
	if got := rulePgCronHistoryUnbounded(nil, nil, nil, cronExtras(unread)); len(got) != 0 {
		t.Fatalf("an unread history size emitted %v", got)
	}
}

// The four extension categories are rules a rule pack can name.
func TestExtRules_RegisteredAndDisableable(t *testing.T) {
	names := map[string]bool{}
	for _, spec := range AllRules {
		names[spec.Name] = true
	}
	for _, c := range extCategories {
		if !names[c] {
			t.Errorf("rule %s is not in AllRules", c)
		}
		d, err := findingcat.ResolveDisabled([]string{c})
		if err != nil || !d.Rules[c] || !d.Categories[c] {
			t.Errorf("ResolveDisabled(%s) = %+v, %v", c, d, err)
		}
		if !slices.Contains(Categories(), c) {
			t.Errorf("%s is not an analyzer category", c)
		}
	}
	if len(extCategories) != 4 {
		t.Fatalf("extCategories = %v, want the four extension categories", extCategories)
	}
}
