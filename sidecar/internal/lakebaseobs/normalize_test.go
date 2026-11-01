package lakebaseobs

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPlanNormalizationPreservesProvenanceAndQueryID(t *testing.T) {
	e := eventFor(t, Plans, time.Now().Add(-time.Hour))
	e.Fields["queryid"] = "18446744073709551615"
	e.Fields["plan_fingerprint"] = "97"
	p, err := NormalizePlan(e)
	if err != nil {
		t.Fatal(err)
	}
	if p.QueryID != -1 || p.AnalyzeEnabled == nil || *p.AnalyzeEnabled || p.BuffersEnabled == nil || !*p.BuffersEnabled || p.TimingEnabled != nil || p.Fingerprint != "97" || p.DurationSource != "lakebase_plan_history.duration_ms" || !p.CapturedAt.Equal(e.OccurredAt) || p.TotalCost != 1.25 || p.ExecutionMS != 12.5 || p.JSON[0] != '[' {
		t.Fatalf("incorrect provenance %+v", p)
	}
	e.Fields["plan_json"] = string(p.JSON)
	if _, err = NormalizePlan(e); err != nil {
		t.Fatalf("array form: %v", err)
	}
}

func TestPlanRejectsUnusableAndSelfEvidence(t *testing.T) {
	cases := []struct {
		name, key, value string
		want             error
	}{
		{"masked", "plan_json", "<REDACTED>", ErrMasked}, {"null", "plan_json", "", ErrMasked}, {"querymask", "query_text", "<REDACTED>", ErrMasked},
		{"truncated", "truncated", "true", ErrMalformed}, {"unknowntruncation", "truncated", "", ErrMalformed}, {"invalid", "plan_json", "{", ErrMalformed},
		{"missingplan", "plan_json", `{"a":1}`, ErrMalformed}, {"multiple", "plan_json", `[{"Plan":{"Node Type":"Result","Total Cost":1}},{"Plan":{}}]`, ErrMalformed},
		{"mismatched-queryid", "plan_json", `{"Query Identifier":18,"Plan":{"Node Type":"Result","Total Cost":1}}`, ErrMalformed},
		{"embedded-self", "plan_json", `{"Query Text":"/* pg_sage */ SELECT 42","Plan":{"Node Type":"Result","Total Cost":1}}`, ErrSelfStatement},
		{"analyze-without-actuals", "analyze_enabled", "true", ErrMalformed},
		{"missingid", "queryid", "0", ErrMalformed}, {"nonfinite", "duration_ms", "NaN", ErrMalformed}, {"negative", "total_cost", "-1", ErrMalformed},
		{"selfapp", "application_name", "pg_sage", ErrSelfStatement}, {"selfquery", "query_text", "/* pg_sage */ SELECT 42", ErrSelfStatement},
		{"oversize", "plan_json", strings.Repeat("x", (1<<20)+1), ErrBudget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := eventFor(t, Plans, time.Now())
			e.Fields[c.key] = c.value
			if _, err := NormalizePlan(e); !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
		})
	}
	e := eventFor(t, Plans, time.Now())
	e.PayloadAllowed = false
	if _, err := NormalizePlan(e); !errors.Is(err, ErrMasked) {
		t.Fatal(err)
	}
}

func TestLogsRetainEventTimeAndRejectPartial(t *testing.T) {
	e := eventFor(t, Logs, time.Now().Add(-time.Hour))
	l, err := NormalizeLog(e)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Timestamp.Equal(e.OccurredAt) || l.SQLState != "40P01" || l.SessionID != "" || l.PID != 91 || l.Database != "fixture" {
		t.Fatalf("log identity/time %+v", l)
	}
	e.Fields["truncated_mask"] = "1"
	if _, err = NormalizeLog(e); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
	e.Fields["truncated_mask"] = "0"
	e.PayloadAllowed = false
	if _, err = NormalizeLog(e); !errors.Is(err, ErrMasked) {
		t.Fatal(err)
	}
}

func TestTypedHistoryDoesNotInventLiveEvidence(t *testing.T) {
	now := time.Now().UTC()
	e := eventFor(t, ASH, now)
	e.Fields["blocking_pid"] = "92"
	e.Fields["wait_event_type"] = "Lock"
	e.Fields["xact_age_ms"] = "12"
	s, err := NormalizeASH(e)
	if err != nil || s.BlockingPID != 92 || s.WaitType != "Lock" || s.TransactionAgeMS == nil || *s.TransactionAgeMS != 12 {
		t.Fatalf("ASH %+v %v", s, err)
	}
	e = eventFor(t, DDL, now)
	e.Fields["command_tag"] = "ALTER TABLE"
	e.Fields["object_name"] = "owned_fixture"
	e.Fields["query_text"] = "sensitive synthetic statement"
	d, err := NormalizeDDL(e)
	if err != nil || d.Command != "ALTER TABLE" || d.ObjectName != "owned_fixture" {
		t.Fatalf("DDL %+v %v", d, err)
	}
	e = eventFor(t, DatabaseCounters, now)
	e.Fields["xact_commit"] = "12"
	e.Fields["xact_rollback"] = "1"
	m, err := NormalizeMetrics(e)
	if err != nil || !m.HistoricalOnly || !m.Counters || m.Values["xact_commit"] != 12 || now.Sub(m.From) != time.Minute {
		t.Fatalf("interval %+v %v", m, err)
	}
	// An absent metric remains absent. There is no available-memory signal.
	e = eventFor(t, DatabaseGauges, now)
	m, err = NormalizeMetrics(e)
	if err != nil || m.Counters || len(m.Values) != 0 {
		t.Fatalf("unknown gauge %+v %v", m, err)
	}
	e = eventFor(t, DatabaseCounters, now)
	e.Fields["previous_ts"] = now.Add(time.Minute).Format(time.RFC3339Nano)
	if _, err = NormalizeMetrics(e); !errors.Is(err, ErrMalformed) {
		t.Fatal(err)
	}
}
