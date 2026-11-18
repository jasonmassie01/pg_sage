package config

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

const gib = float64(1 << 30)

// setting is one settings fact as evid-02b writes it.
func setting(name, value, unit, source string) workpack.SettingFact {
	return workpack.SettingFact{ID: "setting/" + name, Name: name, Value: value, Unit: unit,
		Source: source, Changes: []workpack.SettingChange{}}
}

// stock are PostgreSQL 17's defaults of the settings the rules judge.
func stock() []workpack.SettingFact {
	return []workpack.SettingFact{
		setting("checkpoint_completion_target", "0.9", "", "default"),
		setting("checkpoint_timeout", "300", "s", "default"),
		setting("effective_io_concurrency", "1", "", "default"),
		setting("idle_in_transaction_session_timeout", "0", "ms", "default"),
		setting("max_connections", "100", "", "configuration file"),
		setting("max_wal_size", "1024", "MB", "default"),
		setting("random_page_cost", "4", "", "default"),
		setting("shared_buffers", "524288", "8kB", "configuration file"), // 4GB
		setting("work_mem", "4096", "kB", "default"),
	}
}

// unknownMeasures is a measures block with nothing measured.
func unknownMeasures() workpack.Measures {
	u := "unknown"
	return workpack.Measures{TempBytesPerHour: u, CheckpointsRequested: u,
		CheckpointsTimed: u, WALBytesPerHour: u, PeakClientBackends: u, UsableConnections: u}
}

// fixture is a pack's content for one test.
type fixture struct {
	platform, version string
	settings          []workpack.SettingFact // nil: no settings section
	measures          workpack.Measures
	memoryBytes       any // nil: no provider section
	connections       *workpack.Connections
}

// newFixture is a self-managed PostgreSQL 17 server with 16GB of memory,
// the stock settings and nothing measured.
func newFixture() fixture {
	return fixture{platform: "postgres", version: "17.2", settings: stock(),
		measures: unknownMeasures(), memoryBytes: 16 * gib}
}

func (f fixture) with(name, value, unit, source string) fixture {
	out := make([]workpack.SettingFact, 0, len(f.settings)+1)
	for _, s := range f.settings {
		if s.Name != name {
			out = append(out, s)
		}
	}
	f.settings = append(out, setting(name, value, unit, source))
	return f
}

func (f fixture) pack(t *testing.T) *workpack.Pack {
	t.Helper()
	spec := reviewtest.Spec{Database: "db1", Window: reviewtest.Window(90),
		PGVersion: f.version, Platform: f.platform, Connections: f.connections}
	if f.settings != nil {
		spec.Settings = &workpack.Settings{Status: "ok", Settings: f.settings,
			Measures: f.measures, Hardware: workpack.Hardware{VCPUs: "unknown",
				MemoryBytes: "unknown"}}
	}
	if f.memoryBytes != nil {
		spec.Provider = &workpack.Provider{Status: "ok", Facts: []workpack.ProviderFact{
			{ID: "provider/status", Name: "status", Value: "ok"},
			{ID: "provider/memory_bytes", Name: "memory_bytes", Value: f.memoryBytes}}}
	}
	p, err := reviewtest.Build(spec)
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

var env = candidate.Env{Database: "db1", WindowDays: 90}

// generate runs the generator on f's pack.
func generate(t *testing.T, f fixture) ([]candidate.Candidate, candidate.Coverage) {
	t.Helper()
	cands, cov, err := New().Generate(context.Background(), f.pack(t), env)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return cands, cov
}

// only returns the one candidate of kind, failing otherwise.
func only(t *testing.T, cands []candidate.Candidate, kind string) candidate.Candidate {
	t.Helper()
	var hit []candidate.Candidate
	for _, c := range cands {
		if c.Kind == kind {
			hit = append(hit, c)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("%d candidates of kind %s in %+v, want 1", len(hit), kind, cands)
	}
	return hit[0]
}

func none(t *testing.T, cands []candidate.Candidate, kind string) {
	t.Helper()
	for _, c := range cands {
		if c.Kind == kind {
			t.Fatalf("unexpected %s candidate %+v", kind, c)
		}
	}
}

func hasEvidence(c candidate.Candidate, ids ...string) bool {
	for _, want := range ids {
		found := false
		for _, id := range c.EvidenceIDs {
			found = found || id == want
		}
		if !found {
			return false
		}
	}
	return true
}
