package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// Five-minute time to value (roadmap phase 3): wiring. Every database
// runtime records its install kind (new or existing) before it collects,
// then takes a catalog-only first look at once, stores it, proposes test
// schemas as facts, measures the time to the first finding and, with a
// model configured, adds a short summary. An empty first look waits for
// the analyzer's first finding instead.

const (
	firstFindingPoll        = 15 * time.Second
	firstFindingWait        = time.Hour
	firstLookSummaryTimeout = time.Minute
)

// firstLookTracker measures every database's time to first finding for
// the /metrics endpoint.
var firstLookTracker = onboarding.NewTracker()

// processStarted is when this process started; a database's first runtime
// measures its time to first finding from here, a rebuilt one from its own
// start.
var (
	processStarted    = time.Now()
	firstGenerationMu sync.Mutex
	firstGenerations  = map[string]bool{}
)

func measureFrom(name string, now time.Time) time.Time {
	firstGenerationMu.Lock()
	defer firstGenerationMu.Unlock()
	if firstGenerations[name] {
		return now
	}
	firstGenerations[name] = true
	return processStarted
}

// initOnboarding records whether this is a new install. It runs before the
// runtime collects or starts its trust ramp, which mark an install as
// existing.
func (rt *databaseRuntime) initOnboarding(ctx context.Context) {
	st, err := onboarding.Init(ctx, rt.spec.Pool, rt.spec.Name)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: onboarding record: %v", rt.spec.Name, err)
		return
	}
	if st.InstallKind == onboarding.InstallNew {
		logInfo(rt.spec.Scope, "%s", newInstallLog(rt.spec.Name, rt.cfg.Trust.Level))
	}
}

// newInstallLog is the new-install line; only observation gets the
// read-only explanation, a granted level is stated as it is.
func newInstallLog(name, level string) string {
	msg := fmt.Sprintf("db %q: new install, trust %s", name, level)
	if level == onboarding.LevelObservation {
		msg += " (observation only observes: nothing changes outside the sage schema " +
			"until an operator grants more)"
	}
	return msg
}

// firstLookRun is one database's first look and time-to-first-finding
// measurement.
type firstLookRun struct {
	name, provider string
	pool           *pgxpool.Pool
	started        time.Time
	tracker        *onboarding.Tracker
	facts          *facts.Store
	notify         func(context.Context, facts.Fact)
	summarizer     *firstlook.Summarizer
	model          string
	opts           firstlook.Options
	logf           func(level, format string, args ...any)
}

// startFirstLook runs the first look on the instance worker group.
func (rt *databaseRuntime) startFirstLook() {
	run := &firstLookRun{name: rt.spec.Name, provider: rt.provider, pool: rt.spec.Pool,
		started: measureFrom(rt.spec.Name, time.Now()), tracker: firstLookTracker,
		facts: rt.facts, notify: rt.notifyFact, logf: logStructuredWrapper,
		opts: firstLookOptions(rt.cfg)}
	if rt.llmOn {
		run.summarizer = firstlook.NewSummarizer(rt.generalLLM)
		run.model = rt.generalLLM.Model()
	}
	run.tracker.Start(run.name, run.started)
	rt.start(func() {
		run.runOnce(rt.ctx)
		<-rt.ctx.Done()
		run.tracker.ForgetRun(run.name, run.started)
	})
	rt.note("first-look")
}

// firstLookOptions applies pg_sage's query timeout and the analyzer's
// wraparound thresholds, so the first look and the analyzer agree.
func firstLookOptions(c *config.Config) firstlook.Options {
	th := firstlook.DefaultThresholds()
	if c.Analyzer.XIDWraparoundWarning > 0 && c.Analyzer.XIDWraparoundCritical > 0 {
		th.XIDWarnFraction = firstlook.XIDFraction(c.Analyzer.XIDWraparoundWarning)
		th.XIDCriticalFraction = firstlook.XIDFraction(c.Analyzer.XIDWraparoundCritical)
	}
	return firstlook.Options{StatementTimeout: firstLookTimeout(c.Safety.QueryTimeoutMs),
		Thresholds: th}
}

// firstLookTimeout keeps the first look inside pg_sage's own query timeout.
func firstLookTimeout(queryTimeoutMS int) time.Duration {
	if queryTimeoutMS <= 0 {
		return firstlook.DefaultStatementTimeout
	}
	return min(time.Duration(queryTimeoutMS)*time.Millisecond,
		firstlook.DefaultStatementTimeout)
}

func (f *firstLookRun) runOnce(ctx context.Context) {
	report, err := f.execute(ctx)
	if err != nil {
		f.logf("WARN", "first look of %q: %v", f.name, err)
		return
	}
	f.summarize(ctx, report)
	if len(report.Items) == 0 {
		wctx, cancel := context.WithTimeout(ctx, firstFindingWait)
		defer cancel()
		f.awaitFirstFinding(wctx, firstFindingPoll)
	}
}

// execute takes the first look, stores it, records the first finding and
// proposes the idle test schemas as facts.
func (f *firstLookRun) execute(ctx context.Context) (firstlook.Report, error) {
	opts := f.opts
	opts.Database, opts.Provider = f.name, f.provider
	report, err := firstlook.Run(ctx, f.pool, opts)
	if err != nil {
		return firstlook.Report{}, err
	}
	if report.ID, err = firstlook.NewStore(f.pool).Save(ctx, report); err != nil {
		return report, err
	}
	if err := onboarding.RecordFirstLook(ctx, f.pool, f.name, report.FinishedAt); err != nil {
		f.logf("WARN", "first look of %q: record: %v", f.name, err)
	}
	took := time.Duration(report.DurationMS) * time.Millisecond
	if ttff, first := f.tracker.FirstLook(f.name, report.FinishedAt, len(report.Items),
		took); first {
		f.recordFirstFinding(ctx, report.FinishedAt, ttff, "first_look")
	}
	f.proposeFacts(ctx, report.FactProposals)
	f.logf("INFO", "first look of %q: %d findings over %d relations in %s%s", f.name,
		len(report.Items), report.Relations, took, degradedNote(report.Checks))
	return report, nil
}

func degradedNote(checks []firstlook.Check) string {
	var names []string
	for _, c := range checks {
		if c.Status == firstlook.CheckDegraded {
			names = append(names, c.Rule)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(" (degraded: %s)", strings.Join(names, ", "))
}

func (f *firstLookRun) recordFirstFinding(ctx context.Context, at time.Time,
	ttff time.Duration, source string) {
	if _, err := onboarding.RecordFirstFinding(ctx, f.pool, f.name, at, ttff,
		source); err != nil {
		f.logf("WARN", "first look of %q: record first finding: %v", f.name, err)
	}
	f.logf("INFO", "db %q: first finding (%s) %s after start", f.name, source,
		ttff.Round(time.Millisecond))
}

// proposeFacts records the first look's fact proposals; an operator
// confirms them, so nothing binds yet.
func (f *firstLookRun) proposeFacts(ctx context.Context, props []facts.Proposal) {
	if f.facts == nil {
		return
	}
	for _, p := range props {
		fact, created, err := f.facts.Propose(ctx, p)
		if err != nil {
			f.logf("WARN", "first look of %q: propose %s %q: %v", f.name, p.Type,
				p.Subject, err)
			continue
		}
		if created && f.notify != nil {
			f.notify(ctx, fact)
		}
	}
}

// summarize asks the model for a summary; the findings stand without it.
func (f *firstLookRun) summarize(ctx context.Context, report firstlook.Report) {
	if f.summarizer == nil || len(report.Items) == 0 {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, firstLookSummaryTimeout)
	defer cancel()
	summary, err := f.summarizer.Summarize(sctx, report)
	if err != nil {
		f.logf("WARN", "first look of %q: model summary skipped: %v", f.name, err)
		return
	}
	if err := firstlook.NewStore(f.pool).SetSummary(ctx, report.ID, summary,
		f.model); err != nil {
		f.logf("WARN", "first look of %q: save summary: %v", f.name, err)
	}
}

// awaitFirstFinding polls for the analyzer's first open finding after an
// empty first look; it reports whether one was seen before ctx ended.
func (f *firstLookRun) awaitFirstFinding(ctx context.Context, poll time.Duration) bool {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return false
		}
		open, err := onboarding.HasOpenFinding(ctx, f.pool)
		if err != nil && ctx.Err() == nil {
			f.logf("WARN", "db %q: watch for the first finding: %v", f.name, err)
		}
		if open {
			at := time.Now()
			if ttff, first := f.tracker.FirstFinding(f.name, at); first {
				f.recordFirstFinding(ctx, at, ttff, "analyzer")
			}
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// writeFirstLookMetrics exports each database's first look and time to
// first finding; a value appears once it is known.
func writeFirstLookMetrics(b *strings.Builder, metrics []onboarding.Metrics) {
	type series struct {
		name, help string
		value      func(onboarding.Metrics) (float64, bool)
	}
	all := []series{
		{"pg_sage_time_to_first_finding_seconds", "Seconds from start to the first " +
			"finding (first look or analyzer).", func(m onboarding.Metrics) (float64, bool) {
			return m.TTFF.Seconds(), m.HasTTFF
		}},
		{"pg_sage_first_look_items", "Findings in the catalog-only first look.",
			func(m onboarding.Metrics) (float64, bool) {
				return float64(m.FirstLookItems), m.FirstLookDone
			}},
		{"pg_sage_first_look_duration_seconds", "Duration of the first look.",
			func(m onboarding.Metrics) (float64, bool) {
				return m.FirstLookDuration.Seconds(), m.FirstLookDone
			}},
	}
	for _, s := range all {
		var lines []string
		for _, m := range metrics {
			if v, ok := s.value(m); ok {
				lines = append(lines, fmt.Sprintf("%s{database=%q} %s\n", s.name, m.Database,
					strconv.FormatFloat(v, 'f', -1, 64)))
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", s.name, s.help, s.name)
		b.WriteString(strings.Join(lines, "") + "\n")
	}
}
