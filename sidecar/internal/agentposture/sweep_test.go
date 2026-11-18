package agentposture

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// fleetpol-02a: the exposure sweep and its detectors, without a database.

func TestExposureDetectors_AreTheExposureSubsetAndRegistered(t *testing.T) {
	want := []string{"AP-01", "AP-02", "AP-03", "AP-04", "AP-05", "AP-15", "AP-17"}
	if !slices.Equal(ExposureDetectors, want) {
		t.Fatalf("ExposureDetectors = %v, want %v", ExposureDetectors, want)
	}
	for _, id := range want {
		if _, ok := Default().Get(id); !ok {
			t.Errorf("%s is not registered", id)
		}
	}
}

func TestExposureRegistry_SplitsAnyRegistry(t *testing.T) {
	reg := NewRegistry()
	for _, id := range []string{"AP-01", "AP-03", "AP-06", "AP-13", "AP-17"} {
		if err := reg.Register(&countingDetector{id: id}); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(r *Registry) []string {
		var out []string
		for _, d := range r.Detectors() {
			out = append(out, d.Spec().ID)
		}
		return out
	}
	if got := ids(exposureOnly(reg)); !slices.Equal(got, []string{"AP-01", "AP-03", "AP-17"}) {
		t.Fatalf("exposure subset = %v", got)
	}
	if got := ids(withoutExposure(reg)); !slices.Equal(got, []string{"AP-06", "AP-13"}) {
		t.Fatalf("the rest = %v", got)
	}
	// Nil/empty: an empty registry splits into two empty ones.
	if len(exposureOnly(NewRegistry()).Detectors()) != 0 ||
		len(withoutExposure(nil).Detectors()) != 0 {
		t.Fatal("an empty registry must split into empty registries")
	}
}

// The exposure findings carry the shadow mode, the would-be action of
// fleetpol-02b and the one line on what SQL cannot see.
func TestAnalyzerFinding_ExposureDetail(t *testing.T) {
	cases := map[string]string{"AP-03": "enable_row_level_security",
		"AP-15": "revoke_public_grant", "AP-04": "", "AP-05": "", "AP-17": ""}
	for id, action := range cases {
		a := Finding{Detector: id, Severity: Warning, ObjectType: "table",
			Object: "public.leads", Title: "t"}.AnalyzerFinding()
		if a.Detail["mode"] != ExposureModeShadow {
			t.Errorf("%s mode = %v, want shadow", id, a.Detail["mode"])
		}
		got, has := a.Detail["would_be_action"]
		if action == "" && has {
			t.Errorf("%s would_be_action = %v, want none", id, got)
		}
		if action != "" && got != action {
			t.Errorf("%s would_be_action = %v, want %s", id, got, action)
		}
		line, _ := a.Detail["not_visible"].(string)
		if !strings.Contains(line, "network") || !strings.Contains(line, "key") ||
			strings.Contains(line, "\n") {
			t.Errorf("%s not_visible = %q, want one line on network and keys", id, line)
		}
	}
	for _, id := range []string{"AP-01", "AP-02", "AP-13", "AP-16"} {
		a := Finding{Detector: id, Severity: Warning, ObjectType: "role", Object: "r",
			Title: "t"}.AnalyzerFinding()
		for _, key := range []string{"mode", "would_be_action", "not_visible"} {
			if _, ok := a.Detail[key]; ok {
				t.Errorf("%s carries %s, which only exposure findings carry", id, key)
			}
		}
	}
}

func TestSweepInterval_FallsBackToTheDefault(t *testing.T) {
	cases := map[time.Duration]time.Duration{0: time.Minute, -time.Second: time.Minute,
		14 * time.Second: time.Minute, 601 * time.Second: time.Minute,
		15 * time.Second: 15 * time.Second, 10 * time.Minute: 10 * time.Minute}
	for in, want := range cases {
		if got := sweepInterval(func() time.Duration { return in }); got != want {
			t.Errorf("sweepInterval(%s) = %s, want %s", in, got, want)
		}
	}
	if got := sweepInterval(nil); got != DefaultSweepInterval {
		t.Fatalf("sweepInterval(nil) = %s, want %s", got, DefaultSweepInterval)
	}
}

// fakeClock hands out one channel per wait and records the waits.
type fakeClock struct {
	mu    sync.Mutex
	waits []time.Duration
	ticks chan time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{ticks: make(chan time.Time)} }

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	return c.ticks
}

func (c *fakeClock) waited() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits)
}

// recordingPublisher runs each sweep like the analyzer would and keeps
// what it published.
type recordingPublisher struct {
	mu   sync.Mutex
	runs []analyzer.OutOfCycle
	ran  []bool
	err  error
	seen chan struct{}
}

func newRecordingPublisher() *recordingPublisher {
	return &recordingPublisher{seen: make(chan struct{}, 64)}
}

func (p *recordingPublisher) RunOutOfCycle(ctx context.Context,
	fn analyzer.OutOfCycleRun) error {
	run, ran, err := fn(ctx)
	p.mu.Lock()
	p.runs, p.ran = append(p.runs, run), append(p.ran, ran)
	perr := p.err
	p.mu.Unlock()
	p.seen <- struct{}{}
	if err != nil {
		return err
	}
	if ran {
		return perr
	}
	return nil
}

func (p *recordingPublisher) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.seen:
	case <-time.After(30 * time.Second):
		t.Fatal("no sweep was published")
	}
}

func (p *recordingPublisher) last() (analyzer.OutOfCycle, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runs[len(p.runs)-1], p.ran[len(p.ran)-1]
}

// Fail closed: without a publisher a sweep could only record that the
// exposure detectors ran while their findings went nowhere, so the
// sweeper does not start at all.
func TestRunSweeps_WithoutPublisherDoesNothing(t *testing.T) {
	m := NewMonitor(nil, MonitorOptions{})
	clock := newFakeClock()
	done := make(chan struct{})
	go func() {
		m.RunSweeps(context.Background(), SweepOptions{After: clock.after})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweeps without a publisher did not return")
	}
	if len(clock.waited()) != 0 {
		t.Fatalf("waits = %v, want none", clock.waited())
	}
}

// Error propagation: a sweep without a pool reports ErrNoPool to its
// publisher and the loop keeps going; the live interval is read before
// every wait; the loop ends with its context.
func TestRunSweeps_SweepsAtOnceThenEveryInterval(t *testing.T) {
	m := NewMonitor(nil, MonitorOptions{})
	pub := newRecordingPublisher()
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	interval := 20 * time.Second
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		m.RunSweeps(ctx, SweepOptions{Publisher: pub, After: clock.after,
			Interval: func() time.Duration { mu.Lock(); defer mu.Unlock(); return interval }})
		close(done)
	}()
	pub.wait(t) // enrollment: the first sweep does not wait
	if _, ran := pub.last(); ran {
		t.Fatal("a sweep without a pool reported a run")
	}
	mu.Lock()
	interval = 45 * time.Second
	mu.Unlock()
	clock.ticks <- time.Now()
	pub.wait(t)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweeps did not stop with its context")
	}
	w := clock.waited()
	if len(w) < 2 || w[0] != 20*time.Second || w[1] != 45*time.Second {
		t.Fatalf("waits = %v, want 20s then 45s", w)
	}
}

func TestSweep_NoPool(t *testing.T) {
	_, ran, err := NewMonitor(nil, MonitorOptions{}).Sweep(context.Background())
	if !errors.Is(err, ErrNoPool) || ran {
		t.Fatalf("Sweep = ran %v, %v; want ErrNoPool", ran, err)
	}
}

func TestFailedCategories_AreSortedPostureCategories(t *testing.T) {
	got := failedCategories(map[string]error{"AP-17": errors.New("x"),
		"AP-03": errors.New("y")})
	if !slices.Equal(got, []string{"agent_posture:AP-03", "agent_posture:AP-17"}) {
		t.Fatalf("failedCategories = %v", got)
	}
	if len(failedCategories(nil)) != 0 {
		t.Fatal("no failures must give no categories")
	}
}

// AP-17 without a database: which session roles it flags, and how.
func TestAP17Reasons(t *testing.T) {
	cases := []struct {
		name string
		s    sessionRole
		want []string
	}{
		{"plain role", sessionRole{name: "app"}, nil},
		{"owner", sessionRole{name: "app", owned: 3},
			[]string{"owns 3 relations in user schemas"}},
		{"bypassrls", sessionRole{name: "svc", bypass: true}, []string{"has BYPASSRLS"}},
		{"superuser", sessionRole{name: "root", super: true}, []string{"is a superuser"}},
		{"service_role", sessionRole{name: "service_role"},
			[]string{"is Supabase's service_role"}},
		{"all of them", sessionRole{name: "service_role", super: true, bypass: true,
			owned: 1}, []string{"is a superuser", "has BYPASSRLS",
			"is Supabase's service_role", "owns 1 relation in user schemas"}},
	}
	for _, c := range cases {
		if got := c.s.reasons(); !slices.Equal(got, c.want) {
			t.Errorf("%s: reasons = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAP17Severity_ProductionUnlessVerifiedOtherwise(t *testing.T) {
	s := sessionRole{name: "owner", owned: 1, apps: []string{"mcp-server"}}
	if f := ap17Finding(Env{}, s); f.Severity != Critical {
		t.Fatalf("default severity = %s, want critical (an unlabelled database is prod)",
			f.Severity)
	}
	f := ap17Finding(Env{Config: Config{NonProduction: true}}, s)
	if f.Severity != Warning {
		t.Fatalf("non-production severity = %s, want warning", f.Severity)
	}
	if f.ObjectType != "role" || f.Object != "owner" || f.Title == "" ||
		f.FixScript == "" || f.Recommendation == "" || len(f.Evidence) == 0 {
		t.Fatalf("AP-17 finding = %+v, want a complete role finding", f)
	}
	requireContains(t, "AP-17 detail", f.Detail, "owner", "mcp-server", "owns 1 relation")
}
