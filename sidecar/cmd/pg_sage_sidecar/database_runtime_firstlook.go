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
	// firstLookRetryDelay lets pg_sage's own startup reads (collector,
	// analyzer, self-configuration evidence) settle before the checks that
	// degraded with a transient error run once more.
	firstLookRetryDelay = 30 * time.Second
	// queryTimeoutKey limits the first look only when the operator set it.
	queryTimeoutKey = "safety.query_timeout_ms"
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
	// life is the runtime's context: a re-check ends with the runtime.
	// reprobe asks the collector to re-probe its capabilities before a
	// re-check (nil: no collector). gate admits one pass at a time (the
	// startup first look or a re-check); flight shares a re-check's pass
	// among the operators who asked at once (firstlook-05).
	life    context.Context
	reprobe func(context.Context)
	gate    passGate
	flight  recheckFlight
}

// startFirstLook runs the first look on the instance worker group.
func (rt *databaseRuntime) startFirstLook() {
	run := &firstLookRun{name: rt.spec.Name, provider: rt.provider, pool: rt.spec.Pool,
		started: measureFrom(rt.spec.Name, time.Now()), tracker: firstLookTracker,
		facts: rt.facts, notify: rt.notifyFact, logf: logStructuredWrapper,
		opts: rt.resolveFirstLookOptions(rt.ctx), life: rt.ctx,
		reprobe: rt.reprobeCapabilities}
	if rt.llmOn {
		run.summarizer = firstlook.NewSummarizer(rt.generalLLM)
		run.model = rt.generalLLM.Model()
	}
	run.tracker.Start(run.name, run.started)
	rt.firstLook = run
	rt.start(func() {
		run.runOnce(rt.ctx)
		<-rt.ctx.Done()
		run.tracker.ForgetRun(run.name, run.started)
	})
	rt.note("first-look")
}

// resolveFirstLookOptions reads which settings the operator set; when that
// cannot be read, the configured query timeout is kept (fail closed).
func (rt *databaseRuntime) resolveFirstLookOptions(ctx context.Context) firstlook.Options {
	set, err := operatorSetKeys(ctx, rt.cfg.ConfigPath, rt.spec.Name, configControlPool(),
		rt.spec.DatabaseID)
	if err != nil {
		logWarn(rt.spec.Scope, "db %q: first look keeps %s (%d ms): cannot read which "+
			"settings the operator set: %v", rt.spec.Name, queryTimeoutKey,
			rt.cfg.Safety.QueryTimeoutMs, err)
		set = nil
	}
	return firstLookOptions(rt.cfg, set)
}

// firstLookOptions applies the analyzer's wraparound thresholds, so the
// first look and the analyzer agree, and the first look's own statement
// budget: safety.query_timeout_ms lowers it only when the operator set it
// (operatorSet; nil when unknown, which keeps it), never pg_sage's default.
func firstLookOptions(c *config.Config, operatorSet map[string]bool) firstlook.Options {
	th := firstlook.DefaultThresholds()
	if c.Analyzer.XIDWraparoundWarning > 0 && c.Analyzer.XIDWraparoundCritical > 0 {
		th.XIDWarnFraction = firstlook.XIDFraction(c.Analyzer.XIDWraparoundWarning)
		th.XIDCriticalFraction = firstlook.XIDFraction(c.Analyzer.XIDWraparoundCritical)
	}
	posture := postureConfig(c)
	opts := firstlook.Options{StatementTimeout: firstlook.DefaultStatementTimeout,
		Thresholds: th, Posture: &posture}
	limit := time.Duration(c.Safety.QueryTimeoutMs) * time.Millisecond
	operator := operatorSet == nil || operatorSet[queryTimeoutKey]
	if operator && limit > 0 && limit < opts.StatementTimeout {
		opts.StatementTimeout, opts.TimeoutSetBy = limit, queryTimeoutKey
	}
	return opts
}

func (f *firstLookRun) runOnce(ctx context.Context) {
	report, ok := f.startupPass(ctx)
	if !ok {
		return
	}
	if len(report.Items) == 0 {
		wctx, cancel := context.WithTimeout(ctx, firstFindingWait)
		defer cancel()
		f.awaitFirstFinding(wctx, firstFindingPoll)
	}
}

// startupPass is the startup first look, its retry and its summary; a
// re-check that arrives meanwhile waits for it. ok is false when it failed
// or ctx ended.
func (f *firstLookRun) startupPass(ctx context.Context) (firstlook.Report, bool) {
	if err := f.gate.acquire(ctx); err != nil {
		return firstlook.Report{}, false
	}
	defer f.gate.release()
	report, err := f.execute(ctx)
	if err != nil {
		f.logf("WARN", "first look of %q: %v", f.name, err)
		return firstlook.Report{}, false
	}
	report = f.retryDegraded(ctx, report, firstLookRetryDelay)
	if ctx.Err() != nil {
		return firstlook.Report{}, false
	}
	return f.summarize(ctx, report), true
}

// execute takes the startup first look: catalogPass measured by the
// time-to-first-finding tracker.
func (f *firstLookRun) execute(ctx context.Context) (firstlook.Report, error) {
	return f.catalogPass(ctx, f.tracker)
}

// catalogPass takes the first look, stores it, records the first finding
// in tracker (nil for a re-check, which never sets it) and proposes the
// idle test schemas as facts.
func (f *firstLookRun) catalogPass(ctx context.Context, tracker *onboarding.Tracker) (
	firstlook.Report, error) {
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
	if ttff, first := tracker.FirstLook(f.name, report.FinishedAt, len(report.Items),
		took); first {
		f.recordFirstFinding(ctx, report.FinishedAt, ttff, "first_look")
	}
	f.proposeFacts(ctx, report.FactProposals)
	f.logf("INFO", "first look of %q: %d findings over %d relations in %s%s", f.name,
		len(report.Items), report.Relations, took, degradedNote(report.Checks))
	return report, nil
}

// retryDegraded runs once more, after delay, the checks that degraded with
// a transient error, and records the outcome on the stored report, the
// metrics and the first finding. The first attempt stands when ctx ends or
// the retry fails.
func (f *firstLookRun) retryDegraded(ctx context.Context, report firstlook.Report,
	delay time.Duration) firstlook.Report {
	return f.retryWith(ctx, report, delay, f.tracker)
}

// retryWith is retryDegraded measured by tracker (nil for a re-check).
func (f *firstLookRun) retryWith(ctx context.Context, report firstlook.Report,
	delay time.Duration, tracker *onboarding.Tracker) firstlook.Report {
	if len(report.Retryable) == 0 {
		return report
	}
	f.logf("INFO", "first look of %q: retrying %s in %s", f.name,
		strings.Join(report.Retryable, ", "), delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return report
	case <-timer.C:
	}
	opts := f.opts
	opts.Database, opts.Provider = f.name, f.provider
	retried, err := firstlook.Retry(ctx, f.pool, opts, report)
	if err != nil {
		f.logf("WARN", "first look of %q: retry of degraded checks: %v", f.name, err)
		return report
	}
	if err := firstlook.NewStore(f.pool).Update(ctx, retried); err != nil {
		f.logf("WARN", "first look of %q: record the retry: %v", f.name, err)
	}
	at := time.Now()
	took := time.Duration(retried.DurationMS) * time.Millisecond
	if ttff, first := tracker.FirstLook(f.name, at, len(retried.Items), took); first {
		f.recordFirstFinding(ctx, at, ttff, "first_look")
	}
	f.proposeFacts(ctx, retried.FactProposals)
	f.logf("INFO", "first look of %q: after one retry, %d findings%s", f.name,
		len(retried.Items), degradedNote(retried.Checks))
	return retried
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

// summarize asks the model for a summary and returns report with it; the
// findings stand without it. An unchanged first look reuses the previous
// report's summary with no model call (reuseSummary). The summarizer
// decides whether there is anything to summarize (findings or checks that
// could not run); an empty summary is not saved.
func (f *firstLookRun) summarize(ctx context.Context,
	report firstlook.Report) firstlook.Report {
	if f.summarizer == nil {
		return report
	}
	if reused, ok := f.reuseSummary(ctx, report); ok {
		return reused
	}
	sctx, cancel := context.WithTimeout(ctx, firstLookSummaryTimeout)
	defer cancel()
	summary, err := f.summarizer.Summarize(sctx, report)
	if err != nil {
		f.logf("WARN", "first look of %q: model summary skipped: %v", f.name, err)
		return report
	}
	if summary == "" {
		return report
	}
	if err := firstlook.NewStore(f.pool).SetSummary(ctx, report.ID, summary,
		f.model); err != nil {
		f.logf("WARN", "first look of %q: save summary: %v", f.name, err)
		return report
	}
	report.Summary, report.SummaryModel = summary, f.model
	return report
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
				var line strings.Builder
				writeSample(&line, s.name, dbLabel(m.Database),
					strconv.FormatFloat(v, 'f', -1, 64))
				lines = append(lines, line.String())
			}
		}
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", s.name, s.help, s.name)
		b.WriteString(strings.Join(lines, "") + "\n")
	}
}
