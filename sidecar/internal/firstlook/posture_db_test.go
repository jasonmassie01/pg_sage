package firstlook

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentposture"
)

// postureFake is a posture detector whose statement and arms a test sets;
// it reports one finding per row its statement returns.
type postureFake struct {
	id   string
	sql  string
	arms []agentposture.Arm
}

func (d postureFake) Spec() agentposture.Spec {
	return agentposture.Spec{ID: d.id, Title: "fake " + d.id,
		Severity: agentposture.Warning, Arms: d.arms}
}

func (d postureFake) Detect(ctx context.Context, in agentposture.Input) (
	[]agentposture.Finding, error) {
	rows, err := in.Q.Query(ctx, agentposture.Statement(d.id, d.sql))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agentposture.Finding
	for rows.Next() {
		var obj string
		if err := rows.Scan(&obj); err != nil {
			return nil, err
		}
		out = append(out, agentposture.Finding{Severity: agentposture.Warning,
			ObjectType: "test", Object: obj, Title: "exposed " + obj,
			FixScript: "REVOKE ALL ON " + obj + " FROM PUBLIC;"})
	}
	return out, rows.Err()
}

func postureOptions(t *testing.T, dets ...agentposture.Detector) Options {
	t.Helper()
	reg := agentposture.NewRegistry()
	for _, d := range dets {
		if err := reg.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	opts := testOptions("app")
	opts.PostureRegistry = reg
	return opts
}

func postureChecks(r Report) []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Section == SectionAgentPosture {
			out = append(out, c)
		}
	}
	return out
}

func TestRun_AgentPostureSection(t *testing.T) {
	pool, ctx := livePool(t)
	opts := postureOptions(t,
		postureFake{id: "AP-01", sql: "SELECT 'public.a' UNION ALL SELECT 'public.b'"},
		postureFake{id: "AP-02", sql: "SELECT 'x' WHERE false",
			arms: []agentposture.Arm{{Name: "future", MinVersion: 990000,
				SkipReason: "PostgreSQL 99+ only"}}})
	r, err := Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	pc := postureChecks(r)
	if len(pc) != 2 || pc[0].Rule != "AP-01" || pc[1].Rule != "AP-02" {
		t.Fatalf("posture checks = %+v, want AP-01 and AP-02 in order", pc)
	}
	if pc[0].Status != CheckFinding || pc[1].Status != CheckOK {
		t.Fatalf("statuses = %s, %s", pc[0].Status, pc[1].Status)
	}
	if !strings.Contains(pc[1].Note, "arm future skipped: PostgreSQL 99+ only") {
		t.Fatalf("AP-02 note = %q, want the recorded skip reason", pc[1].Note)
	}
	it := findItem(r, "AP-01", "public.b")
	if it == nil || it.Section != SectionAgentPosture ||
		it.SuggestedSQL != "REVOKE ALL ON public.b FROM PUBLIC;" {
		t.Fatalf("posture item = %+v", it)
	}
	// The catalog checks are untouched and carry no section.
	if c := checkStatus(r, RuleXIDRunway); c.Status == "" || c.Section != "" {
		t.Fatalf("xid check = %+v", c)
	}
}

func TestRun_FailingPostureDetectorDegradesOnlyItself(t *testing.T) {
	pool, ctx := livePool(t)
	opts := postureOptions(t,
		postureFake{id: "AP-01", sql: "SELECT * FROM no_such_posture_table"},
		postureFake{id: "AP-02", sql: "SELECT 'public.c'"})
	r, err := Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c := checkStatus(r, "AP-01"); c.Status != CheckDegraded ||
		!strings.Contains(c.Note, "no_such_posture_table") || c.Section != SectionAgentPosture {
		t.Fatalf("AP-01 = %+v, want degraded with the reason", c)
	}
	if c := checkStatus(r, "AP-02"); c.Status != CheckFinding {
		t.Fatalf("AP-02 = %+v, want its finding despite AP-01's failure", c)
	}
	if slices.Contains(r.Retryable, "AP-01") {
		t.Fatal("a missing relation is not transient, so it must not be retried")
	}
	for _, rule := range []string{RuleXIDRunway, RuleMissingExtension} {
		if c := checkStatus(r, rule); c.Status == CheckDegraded {
			t.Fatalf("%s degraded by a posture failure: %+v", rule, c)
		}
	}
}

func TestRun_PostureTimeoutIsRetried(t *testing.T) {
	pool, ctx := livePool(t)
	opts := postureOptions(t,
		postureFake{id: "AP-01", sql: "SELECT 'slow' FROM pg_sleep(0.6)"},
		postureFake{id: "AP-02", sql: "SELECT 'fast'"})
	opts.StatementTimeout = 400 * time.Millisecond
	r, err := Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if c := checkStatus(r, "AP-01"); c.Status != CheckDegraded ||
		!strings.Contains(c.Note, "statement timeout") {
		t.Fatalf("AP-01 = %+v, want a statement timeout", c)
	}
	if !slices.Contains(r.Retryable, "AP-01") || slices.Contains(r.Retryable, "AP-02") {
		t.Fatalf("retryable = %v, want AP-01 only", r.Retryable)
	}
	opts.StatementTimeout = 2 * time.Second
	retried, err := Retry(ctx, pool, opts, r)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	c := checkStatus(retried, "AP-01")
	if c.Status != CheckFinding || !c.Retried || !strings.Contains(c.Note, "first attempt") ||
		c.Section != SectionAgentPosture {
		t.Fatalf("retried AP-01 = %+v", c)
	}
	if findItem(retried, "AP-01", "slow") == nil {
		t.Fatalf("AP-01 finding missing after the retry: %+v", retried.Items)
	}
	if n := countItems(retried, "AP-02"); n != 1 {
		t.Fatalf("AP-02 items = %d after the retry, want 1 (not duplicated)", n)
	}
}

func countItems(r Report, rule string) int {
	n := 0
	for _, it := range r.Items {
		if it.Rule == rule {
			n++
		}
	}
	return n
}

func TestRun_InvalidPostureConfigDegradesEveryPostureCheck(t *testing.T) {
	pool, ctx := livePool(t)
	opts := postureOptions(t, postureFake{id: "AP-01", sql: "SELECT 'a'"},
		postureFake{id: "AP-02", sql: "SELECT 'b'"})
	bad := agentposture.DefaultConfig()
	bad.ClientPatterns = []string{"not-anchored"}
	opts.Posture = &bad
	r, err := Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, c := range postureChecks(r) {
		if c.Status != CheckDegraded || !strings.Contains(c.Note, "anchored") {
			t.Fatalf("posture check %+v, want degraded naming the config error", c)
		}
	}
	if len(postureChecks(r)) != 2 || len(r.Retryable) != 0 {
		t.Fatalf("checks %+v retryable %v", r.Checks, r.Retryable)
	}
}

func TestRun_PostureItemsAreCapped(t *testing.T) {
	pool, ctx := livePool(t)
	opts := postureOptions(t, postureFake{id: "AP-01",
		sql: "SELECT 'obj' || g FROM generate_series(1, 7) g"})
	opts.Thresholds.MaxItemsPerRule = 5
	r, err := Run(ctx, pool, opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := countItems(r, "AP-01"); n != 5 {
		t.Fatalf("AP-01 items = %d, want 5", n)
	}
	if c := checkStatus(r, "AP-01"); !strings.Contains(c.Note, "2 more not shown") {
		t.Fatalf("AP-01 note = %q", c.Note)
	}
}

// The default registry runs when no registry is given: every registered
// detector gets a check in the section.
func TestRun_DefaultRegistryRunsEveryDetector(t *testing.T) {
	pool, ctx := livePool(t)
	r, err := Run(ctx, pool, testOptions("app"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var want []string
	for _, d := range agentposture.Default().Detectors() {
		want = append(want, d.Spec().ID)
	}
	var got []string
	for _, c := range postureChecks(r) {
		got = append(got, c.Rule)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("posture checks = %v, want %v", got, want)
	}
}
