package connections

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

// t0 is Saturday 2026-10-31 12:00 UTC, inside reviewtest.Window(90): hour
// of week (6-1)*24+12 = 132.
var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

const t0Hour = 132

func hourID(how int) string { return "connections/hour/" + strconv.Itoa(how) }

// allHours is a connections section with its 168 hours.
func allHours() *workpack.Connections {
	c := &workpack.Connections{Hours: make([]workpack.ConnectionHour, 168)}
	for i := range c.Hours {
		c.Hours[i] = workpack.ConnectionHour{ID: hourID(i), Hour: i, States: "unknown",
			Ages: "unknown"}
	}
	return c
}

// measures is a settings section's measures: usable connections and the
// window's peak client backends (nil: unknown).
func measures(usable, peak any) workpack.Measures {
	u := "unknown"
	m := workpack.Measures{TempBytesPerHour: u, CheckpointsRequested: u, CheckpointsTimed: u,
		WALBytesPerHour: u, PeakClientBackends: u, UsableConnections: u}
	if usable != nil {
		m.UsableConnections = usable
	}
	if peak != nil {
		m.PeakClientBackends = peak
	}
	return m
}

// packWith seals a 90-day pack with all 168 connection hours and a
// settings section holding max_connections and m.
func packWith(t *testing.T, m workpack.Measures) *workpack.Pack {
	t.Helper()
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1", Window: reviewtest.Window(90),
		Connections: allHours(), Settings: &workpack.Settings{Status: "ok",
			Settings: []workpack.SettingFact{{ID: "setting/max_connections",
				Name: "max_connections", Value: "100", Source: "configuration file",
				Changes: []workpack.SettingChange{}}}, Measures: m}})
	if err != nil {
		t.Fatalf("build pack: %v", err)
	}
	return p
}

// plainPack is packWith nothing measured.
func plainPack(t *testing.T) *workpack.Pack { return packWith(t, measures(nil, nil)) }

func env() candidate.Env { return candidate.Env{Database: "db1", WindowDays: 90} }

// build runs Build and fails the test on an error.
func build(t *testing.T, p *workpack.Pack, in Inputs) ([]candidate.Candidate,
	candidate.Coverage) {
	t.Helper()
	cands, cov, err := Build(p, in, env())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cands, cov
}

// minute is a pointer to m.
func minute(m int64) *int64 { return &m }

// agesAt is one sample's age rows of app: each start counted in the whole
// minute before at it connected, as the collector buckets it.
func agesAt(at time.Time, app string, starts []time.Time) []collector.WorkloadAppAge {
	by := map[int64]int{}
	var order []int64
	for _, s := range starts {
		m := int64(at.Sub(s) / time.Minute)
		if at.Before(s) {
			m = -1
		}
		if by[m] == 0 {
			order = append(order, m)
		}
		by[m]++
	}
	out := make([]collector.WorkloadAppAge, 0, len(order))
	for _, m := range order {
		out = append(out, collector.WorkloadAppAge{Application: app, Minute: minute(m),
			Sessions: by[m]})
	}
	return out
}

// sample is app's sessions (connected at starts) seen at at.
func sample(at time.Time, app string, starts []time.Time) Sample {
	return Sample{At: at, Apps: map[string]int{app: len(starts)},
		Ages: agesAt(at, app, starts)}
}

// synced is n starts spread over the 50 seconds before end, so a sample
// taken a whole number of minutes after end sees them in one minute.
func synced(end time.Time, n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = end.Add(-time.Duration(i) * 50 * time.Second / time.Duration(n))
	}
	return out
}

// staggered is n starts one per minute, the newest at end.
func staggered(end time.Time, n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = end.Add(-time.Duration(i) * time.Minute)
	}
	return out
}

// hourly is three samples one hour apart from t0 of the same sessions.
func hourly(app string, starts []time.Time) []Sample {
	var out []Sample
	for k := 0; k < 3; k++ {
		out = append(out, sample(t0.Add(time.Duration(k)*time.Hour), app, starts))
	}
	return out
}

func ofKind(cands []candidate.Candidate, kind string) []candidate.Candidate {
	var out []candidate.Candidate
	for _, c := range cands {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func only(t *testing.T, cands []candidate.Candidate, kind string) candidate.Candidate {
	t.Helper()
	got := ofKind(cands, kind)
	if len(got) != 1 {
		t.Fatalf("%d %s candidates in %+v, want 1", len(got), kind, cands)
	}
	return got[0]
}

func hasNote(cov candidate.Coverage, parts ...string) bool {
	for _, n := range cov.Notes {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(n, p)
		}
		if all {
			return true
		}
	}
	return false
}

// plainRole is a role pg_sage may budget, with no limit or settings.
func plainRole() budget.RoleFact {
	return budget.RoleFact{ConnLimit: -1, Settings: map[string]string{}}
}

// fakeSource returns in (or err) and records the window it was asked for.
type fakeSource struct {
	in       Inputs
	err      error
	from, to time.Time
	calls    int
}

func (f *fakeSource) Read(_ context.Context, from, to time.Time) (Inputs, error) {
	f.calls++
	f.from, f.to = from, to
	return f.in, f.err
}
