package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// probeAnswer is one scripted reply to a provider probe statement.
type probeAnswer struct {
	value *string
	err   error
	delay time.Duration
}

// scriptedProbeQuerier answers each probe by a fragment of its SQL; a
// probe with no script answers NULL (a setting this server does not have).
type scriptedProbeQuerier struct {
	mu      sync.Mutex
	answers map[string]probeAnswer
	calls   []string
}

func (q *scriptedProbeQuerier) QueryRow(ctx context.Context, sql string, _ ...any) pgx.Row {
	q.mu.Lock()
	q.calls = append(q.calls, sql)
	answer := probeAnswer{}
	for fragment, a := range q.answers {
		if strings.Contains(sql, fragment) {
			answer = a
		}
	}
	q.mu.Unlock()
	return probeRow{ctx: ctx, answer: answer, answerAt: time.Now().Add(answer.delay)}
}

func (q *scriptedProbeQuerier) callCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.calls)
}

// probeRow answers at answerAt unless the caller's deadline comes first.
// The order is decided by the times, not by which timer a loaded scheduler
// happens to run first: a late goroutine sees both timers ready at once.
type probeRow struct {
	ctx      context.Context
	answer   probeAnswer
	answerAt time.Time
}

func (r probeRow) Scan(dest ...any) error {
	if r.answer.delay > 0 {
		if deadline, ok := r.ctx.Deadline(); ok && deadline.Before(r.answerAt) {
			<-r.ctx.Done()
			return r.ctx.Err()
		}
		timer := time.NewTimer(time.Until(r.answerAt))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.ctx.Done():
			if time.Now().Before(r.answerAt) {
				return r.ctx.Err()
			}
		}
	}
	if r.answer.err != nil {
		return r.answer.err
	}
	*(dest[0].(**string)) = r.answer.value
	return nil
}

func textValue(s string) *string { return &s }

func pgError(code string) error {
	return &pgconn.PgError{Code: code, Message: "probe failed with SQLSTATE " + code}
}

func undefinedFunction() error {
	return &pgconn.PgError{Code: "42883", Message: "function aurora_version() does not exist"}
}

// noWaitDetector records the startup backoff instead of sleeping.
func noWaitDetector(q providerQuerier, waits *[]time.Duration) providerDetector {
	d := newProviderDetector(q)
	d.sleep = func(ctx context.Context, wait time.Duration) error {
		*waits = append(*waits, wait)
		return ctx.Err()
	}
	return d
}

func TestProviderProbeOnceClassifiesEachProvider(t *testing.T) {
	cases := []struct {
		name    string
		answers map[string]probeAnswer
		want    string
	}{
		{"aurora", map[string]probeAnswer{"aurora_version": {value: textValue("3.04")}}, "aurora"},
		{"rds", map[string]probeAnswer{
			"aurora_version": {err: undefinedFunction()},
			"rds.extensions": {value: textValue("pg_stat_statements")}}, "rds"},
		{"alloydb", map[string]probeAnswer{
			"aurora_version":             {err: undefinedFunction()},
			"alloydb.iam_authentication": {value: textValue("off")}}, "alloydb"},
		{"cloud-sql", map[string]probeAnswer{
			"aurora_version":              {err: undefinedFunction()},
			"cloudsql.iam_authentication": {value: textValue("off")}}, "cloud-sql"},
		{"azure", map[string]probeAnswer{
			"aurora_version":   {err: undefinedFunction()},
			"azure.extensions": {value: textValue("")}}, "azure"},
		{"self-managed", map[string]probeAnswer{
			"aurora_version": {err: undefinedFunction()}}, "self-managed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &scriptedProbeQuerier{answers: tc.answers}
			got, err := newProviderDetector(q).probeOnce(context.Background())
			if err != nil || got != tc.want {
				t.Fatalf("probeOnce = %q, %v; want %q, nil", got, err, tc.want)
			}
		})
	}
}

// The probe list and its order are a compatibility contract: aurora first
// (Aurora also answers the rds.* settings), azure last.
func TestProviderProbeOrderIsUnchanged(t *testing.T) {
	q := &scriptedProbeQuerier{answers: map[string]probeAnswer{
		"aurora_version": {err: undefinedFunction()}}}
	if _, err := newProviderDetector(q).probeOnce(context.Background()); err != nil {
		t.Fatalf("probeOnce: %v", err)
	}
	want := []string{"aurora_version()", "'rds.extensions'", "'alloydb.iam_authentication'",
		"'cloudsql.iam_authentication'", "'azure.extensions'"}
	if len(q.calls) != len(want) {
		t.Fatalf("probe statements = %q, want %d", q.calls, len(want))
	}
	for i, fragment := range want {
		if !strings.Contains(q.calls[i], fragment) {
			t.Errorf("probe %d = %q, want it to contain %q", i, q.calls[i], fragment)
		}
	}
}

// Any error other than "function does not exist" makes the answer unknown:
// self-managed is the most permissive provider, so it must be earned.
func TestProviderProbeOnceErrorsReadUnknown(t *testing.T) {
	cases := []struct {
		name   string
		answer map[string]probeAnswer
		probe  string
	}{
		{"connection error on the first probe", map[string]probeAnswer{
			"aurora_version": {err: errors.New("dial tcp 10.0.0.5:5432: connection refused")}},
			"aurora"},
		{"query canceled on a setting probe", map[string]probeAnswer{
			"aurora_version": {err: undefinedFunction()},
			"rds.extensions": {err: pgError("57014")}},
			"rds"},
		{"permission denied on the last probe", map[string]probeAnswer{
			"aurora_version":   {err: undefinedFunction()},
			"azure.extensions": {err: pgError("42501")}},
			"azure"},
		{"aurora_version fails for another reason", map[string]probeAnswer{
			"aurora_version": {err: pgError("XX000")}},
			"aurora"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &scriptedProbeQuerier{answers: tc.answer}
			got, err := newProviderDetector(q).probeOnce(context.Background())
			if got != providerUnknown {
				t.Fatalf("provider = %q, want %q", got, providerUnknown)
			}
			if err == nil || !strings.Contains(err.Error(), tc.probe) {
				t.Fatalf("error = %v, want one naming the %s probe", err, tc.probe)
			}
		})
	}
}

func TestProviderDetectRetriesThenSettlesUnknown(t *testing.T) {
	q := &scriptedProbeQuerier{answers: map[string]probeAnswer{
		"aurora_version": {err: errors.New("unexpected EOF")}}}
	var waits []time.Duration
	got, err := noWaitDetector(q, &waits).detect(context.Background())
	if got != providerUnknown || err == nil {
		t.Fatalf("detect = %q, %v; want unknown with an error", got, err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("backoff = %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", waits, want)
		}
	}
	if q.callCount() != 4 {
		t.Fatalf("probe attempts = %d, want 4 (one plus three retries)", q.callCount())
	}
}

// State transition: a transient error at startup is retried, and the retry
// that succeeds gives the real provider.
func TestProviderDetectRecoversOnRetry(t *testing.T) {
	flaky := &flakyProbeQuerier{failures: 2, next: &scriptedProbeQuerier{
		answers: map[string]probeAnswer{"aurora_version": {err: undefinedFunction()},
			"cloudsql.iam_authentication": {value: textValue("on")}}}}
	var waits []time.Duration
	got, err := noWaitDetector(flaky, &waits).detect(context.Background())
	if got != "cloud-sql" || err != nil {
		t.Fatalf("detect = %q, %v; want cloud-sql, nil", got, err)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("backoff = %v, want [1s 2s]", waits)
	}
}

func TestProviderDetectStopsWhenContextEnds(t *testing.T) {
	q := &scriptedProbeQuerier{answers: map[string]probeAnswer{
		"aurora_version": {err: errors.New("connection reset by peer")}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := newProviderDetector(q).detect(ctx)
	if got != providerUnknown || !errors.Is(err, context.Canceled) {
		t.Fatalf("detect = %q, %v; want unknown, context.Canceled", got, err)
	}
	if q.callCount() > 1 {
		t.Fatalf("probe attempts after cancel = %d, want at most 1", q.callCount())
	}
}

// Boundary: each probe has its own 2 s budget. 1.9 s answers in time; 2.1 s
// times out on every attempt and settles on unknown after three retries.
func TestProviderProbeTimeoutBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: the boundary waits for real probe timeouts (~11 s)")
	}
	if providerProbeTimeout != 2*time.Second {
		t.Fatalf("probe timeout = %v, want 2s", providerProbeTimeout)
	}
	slowOK := &scriptedProbeQuerier{answers: map[string]probeAnswer{
		"aurora_version": {value: textValue("3.04"), delay: 1900 * time.Millisecond}}}
	var waits []time.Duration
	if got, err := noWaitDetector(slowOK, &waits).detect(context.Background()); got != "aurora" ||
		err != nil {
		t.Fatalf("1.9 s probe: detect = %q, %v; want aurora, nil", got, err)
	}
	tooSlow := &scriptedProbeQuerier{answers: map[string]probeAnswer{
		"aurora_version": {value: textValue("3.04"), delay: 2100 * time.Millisecond}}}
	waits = nil
	got, err := noWaitDetector(tooSlow, &waits).detect(context.Background())
	if got != providerUnknown || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("2.1 s probe: detect = %q, %v; want unknown, deadline exceeded", got, err)
	}
	if tooSlow.callCount() != 4 || len(waits) != 3 {
		t.Fatalf("2.1 s probe: attempts=%d waits=%v, want 4 attempts and 3 waits",
			tooSlow.callCount(), waits)
	}
}

// Each probe gets a fresh budget: five probes of 1.5 s each (7.5 s in all)
// succeed, which the old shared 5 s context did not allow.
func TestProviderProbeBudgetIsPerProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode: waits for five 1.5 s probes")
	}
	slow := probeAnswer{delay: 1500 * time.Millisecond}
	q := &scriptedProbeQuerier{answers: map[string]probeAnswer{
		"aurora_version": {err: undefinedFunction(), delay: slow.delay},
		"rds.":           slow, "alloydb.": slow, "cloudsql.": slow, "azure.": slow}}
	got, err := newProviderDetector(q).probeOnce(context.Background())
	if got != providerSelfManaged || err != nil {
		t.Fatalf("probeOnce = %q, %v; want self-managed, nil", got, err)
	}
}

func TestDetectCloudEnvNilPoolIsUnknown(t *testing.T) {
	got, err := detectCloudEnvContext(context.Background(), nil)
	if got != providerUnknown || err == nil {
		t.Fatalf("detectCloudEnvContext(nil) = %q, %v; want unknown with an error", got, err)
	}
}

// No concurrent-access test for the detector: it holds no shared state; each
// probe runs on its own pooled connection under its own context.

// flakyProbeQuerier fails whole attempts (the first probe of each) until
// failures run out, then delegates.
type flakyProbeQuerier struct {
	mu       sync.Mutex
	failures int
	next     *scriptedProbeQuerier
}

func (q *flakyProbeQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q.mu.Lock()
	fail := q.failures > 0 && strings.Contains(sql, "aurora_version")
	if fail {
		q.failures--
	}
	q.mu.Unlock()
	if fail {
		closed := errors.New("server closed the connection")
		return probeRow{ctx: ctx, answer: probeAnswer{err: closed}}
	}
	return q.next.QueryRow(ctx, sql, args...)
}

// A YAML fleet starts without a process pool: detectCloudEnv answers
// unknown (fail closed) without probing.
func TestDetectCloudEnvWithoutProcessPoolIsUnknown(t *testing.T) {
	if got := detectCloudEnv(nil); got != providerUnknown {
		t.Fatalf("detectCloudEnv(nil) = %q, want %q", got, providerUnknown)
	}
}
