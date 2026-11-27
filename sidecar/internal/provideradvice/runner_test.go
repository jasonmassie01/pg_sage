package provideradvice

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	name  string
	recs  []Recommendation
	err   error
	calls int
}

func (s *fakeSource) Provider() string { return s.name }

func (s *fakeSource) Fetch(context.Context) ([]Recommendation, error) {
	s.calls++
	return s.recs, s.err
}

type applied struct {
	database, provider string
	rows               []Row
}

type fakeSink struct {
	mu       sync.Mutex
	applies  []applied
	failures map[string]string
	applyErr error
}

func (s *fakeSink) Apply(_ context.Context, database, provider string, rows []Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applies = append(s.applies, applied{database, provider, rows})
	return s.applyErr
}

func (s *fakeSink) RecordSourceError(_ context.Context, database, provider string,
	err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == nil {
		s.failures = map[string]string{}
	}
	s.failures[database+"/"+provider] = err.Error()
	return nil
}

func newTestRunner(t *testing.T, sink *fakeSink, findings func(context.Context) ([]Finding,
	error), sources ...Source) *Runner {
	t.Helper()
	r, err := NewRunner(RunnerOptions{Database: "app", Sources: sources, Sink: sink,
		Findings: findings, Eval: &fakeEval{}})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

func noFindings(context.Context) ([]Finding, error) { return nil, nil }

func TestRunner_SourceErrorIsRecordedAndOtherSourcesStillApply(t *testing.T) {
	sink := &fakeSink{}
	good := &fakeSource{name: "cloud-sql", recs: []Recommendation{
		createRec("p1", "CREATE INDEX ON public.users (email)")}}
	bad := &fakeSource{name: "azure", err: errors.New("HTTP 403 AuthorizationFailed")}
	err := newTestRunner(t, sink, noFindings, bad, good).RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "azure") ||
		!strings.Contains(err.Error(), "403") {
		t.Fatalf("RunOnce error = %v, want the azure source error", err)
	}
	if len(sink.applies) != 1 || sink.applies[0].provider != "cloud-sql" ||
		sink.applies[0].database != "app" || len(sink.applies[0].rows) != 1 {
		t.Fatalf("applies = %+v", sink.applies)
	}
	if sink.failures["app/azure"] != "HTTP 403 AuthorizationFailed" || len(sink.failures) != 1 {
		t.Fatalf("failures = %+v", sink.failures)
	}
}

func TestRunner_FindingsErrorWritesNothing(t *testing.T) {
	sink := &fakeSink{}
	src := &fakeSource{name: "cloud-sql"}
	err := newTestRunner(t, sink, func(context.Context) ([]Finding, error) {
		return nil, errors.New("findings read timed out")
	}, src).RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "findings read timed out") {
		t.Fatalf("err = %v", err)
	}
	if len(sink.applies) != 0 || len(sink.failures) != 0 || src.calls != 0 {
		t.Fatalf("a findings failure must not reconcile: %+v / %+v / %d calls",
			sink.applies, sink.failures, src.calls)
	}
}

func TestRunner_ApplyErrorPropagates(t *testing.T) {
	sink := &fakeSink{applyErr: errors.New("lock timeout")}
	err := newTestRunner(t, sink, noFindings, &fakeSource{name: "supabase"}).
		RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock timeout") ||
		!strings.Contains(err.Error(), "supabase") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunner_RunStopsWithItsContext(t *testing.T) {
	sink := &fakeSink{}
	src := &fakeSource{name: "cloud-sql"}
	r := newTestRunner(t, sink, noFindings, src)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx, time.Hour, func(err error) { t.Errorf("run failed: %v", err) })
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sink.mu.Lock()
		n := len(sink.applies)
		sink.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	if len(sink.applies) != 1 {
		t.Fatalf("Run must reconcile once at start, got %d", len(sink.applies))
	}
}

func TestNewRunner_RejectsIncompleteOptions(t *testing.T) {
	src := &fakeSource{name: "cloud-sql"}
	base := RunnerOptions{Database: "app", Sources: []Source{src}, Sink: &fakeSink{},
		Findings: noFindings, Eval: &fakeEval{}}
	for name, mutate := range map[string]func(*RunnerOptions){
		"database": func(o *RunnerOptions) { o.Database = "" },
		"sources":  func(o *RunnerOptions) { o.Sources = nil },
		"nil src":  func(o *RunnerOptions) { o.Sources = []Source{nil} },
		"sink":     func(o *RunnerOptions) { o.Sink = nil },
		"findings": func(o *RunnerOptions) { o.Findings = nil },
		"eval":     func(o *RunnerOptions) { o.Eval = nil },
		"dup": func(o *RunnerOptions) {
			o.Sources = []Source{src, &fakeSource{name: "cloud-sql"}}
		},
	} {
		o := base
		mutate(&o)
		if _, err := NewRunner(o); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
