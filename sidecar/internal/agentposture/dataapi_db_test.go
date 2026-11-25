package agentposture

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
)

// fleetpol-02c on real PostgreSQL: AP-03 and AP-04 findings on Supabase
// follow the project's Data API exposure, read through a fake Management
// API (cloudtel.SupabaseAPI); no real Supabase project is called.

// scriptedSupabaseAPI serves the schemas it was last given, or an error.
type scriptedSupabaseAPI struct {
	mu      sync.Mutex
	schemas []string
	err     error
}

func (s *scriptedSupabaseAPI) DataAPISettings(context.Context) (
	cloudtel.SupabaseDataAPISettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloudtel.SupabaseDataAPISettings{Schemas: s.schemas}, s.err
}

func (s *scriptedSupabaseAPI) serve(schemas ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemas, s.err = schemas, nil
}

// dataAPIReader is a cloudtel reader over api on a clock the test moves.
type dataAPIReader struct {
	r   *cloudtel.SupabaseDataAPI
	now atomic.Pointer[time.Time]
}

func newDataAPIReader(api cloudtel.SupabaseAPI) *dataAPIReader {
	d := &dataAPIReader{}
	start := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	d.now.Store(&start)
	d.r = cloudtel.NewSupabaseDataAPI(api, cloudtel.SupabaseDataAPIOptions{
		Now: func() time.Time { return *d.now.Load() }})
	return d
}

// refreshAfterAnHour moves the clock past the cache and re-reads.
func (d *dataAPIReader) refreshAfterAnHour(ctx context.Context) {
	next := d.now.Load().Add(time.Hour)
	d.now.Store(&next)
	d.r.Refresh(ctx)
}

// platform is what the runtime hands posture: Supabase and the reader's
// last exposure (nil: unknown).
func (d *dataAPIReader) platform() Platform {
	p := Platform{Provider: "supabase"}
	if e := d.r.Exposure(); e != nil {
		p.DataAPI = &DataAPIExposure{Schemas: e.Schemas}
	}
	return p
}

func onSupabase(p Platform) func(*Env) {
	return func(env *Env) { env.Config.Platform = p }
}

func requireDataAPI(t *testing.T, f Finding, sev Severity, state string) {
	t.Helper()
	if f.Severity != sev || f.DataAPIExposed != state {
		t.Fatalf("%s: severity/data_api_exposed = %s/%q, want %s/%q", f.Object, f.Severity,
			f.DataAPIExposed, sev, state)
	}
}

// Integration: the AP-03 table finding moves unknown (critical) ->
// exposed (critical) -> not exposed (warning) as the fake API changes.
func TestAP03_DataAPIExposureFromFakeSupabaseAPI(t *testing.T) {
	f := newFixture(t)
	tbl := f.q("leads")
	f.exec("CREATE TABLE "+tbl+" (id int, email text)",
		"GRANT SELECT ON "+tbl+" TO "+f.exposed)
	api := &scriptedSupabaseAPI{err: cloudtel.ErrSupabaseUnauthorized}
	reader := newDataAPIReader(api)
	reader.r.Refresh(f.ctx)
	got := requireFinding(t, f.run("AP-03", f.env(onSupabase(reader.platform()))), tbl,
		Critical)
	requireDataAPI(t, got, Critical, DataAPIUnknown)

	api.serve(f.schema, "graphql_public")
	reader.refreshAfterAnHour(f.ctx)
	got = requireFinding(t, f.run("AP-03", f.env(onSupabase(reader.platform()))), tbl,
		Critical)
	requireDataAPI(t, got, Critical, DataAPIExposed)
	if got.AnalyzerFinding().Detail["data_api_exposed"] != DataAPIExposed {
		t.Fatalf("analyzer detail = %v", got.AnalyzerFinding().Detail)
	}

	api.serve("api")
	reader.refreshAfterAnHour(f.ctx)
	got = requireFinding(t, f.run("AP-03", f.env(onSupabase(reader.platform()))), tbl,
		Warning)
	requireDataAPI(t, got, Warning, DataAPINotExposed)
	requireContains(t, "not exposed detail", got.Detail, "does not serve schema "+f.schema)

	// Off Supabase the same table is critical with no Data API field.
	got = requireFinding(t, f.run("AP-03", f.env(nil)), tbl, Critical)
	requireDataAPI(t, got, Critical, "")
}

// A schema with more than groupThreshold exposed tables is one finding;
// it carries the schema's Data API state too.
func TestAP03_SchemaFindingCarriesDataAPIState(t *testing.T) {
	f := newFixture(t)
	for i := range groupThreshold + 1 {
		tbl := f.q(fmt.Sprintf("t%02d", i))
		f.exec("CREATE TABLE "+tbl+" (id int)", "GRANT SELECT ON "+tbl+" TO "+f.exposed)
	}
	o := f.run("AP-03", f.env(onSupabase(supabasePlatform("api"))))
	got := requireFinding(t, o, f.schema, Warning)
	requireDataAPI(t, got, Warning, DataAPINotExposed)
	if got.ObjectType != "schema" {
		t.Fatalf("object type = %s, want schema", got.ObjectType)
	}
	o = f.run("AP-03", f.env(onSupabase(supabasePlatform(f.schema))))
	requireDataAPI(t, requireFinding(t, o, f.schema, Critical), Critical, DataAPIExposed)
}

// AP-04 carries the state; it is already a warning, so not exposed keeps it.
func TestAP04_DataAPIStateOnPolicies(t *testing.T) {
	f := newFixture(t)
	tbl := f.q("docs")
	f.exec("CREATE TABLE "+tbl+" (id int)", "ALTER TABLE "+tbl+" ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY read_all ON "+tbl+" FOR SELECT TO "+f.exposed+" USING (true)")
	o := f.run("AP-04", f.env(onSupabase(supabasePlatform("api"))))
	requireDataAPI(t, requireFinding(t, o, tbl+":read_all", Warning), Warning,
		DataAPINotExposed)
	o = f.run("AP-04", f.env(onSupabase(supabasePlatform())))
	requireDataAPI(t, requireFinding(t, o, tbl+":read_all", Warning), Warning,
		DataAPIUnknown)
}

// State transitions: a change of the Data API state alone (no catalog
// change) makes the next sweep run and changes the severity it publishes;
// the same state again runs nothing.
// It asserts an unchanged state runs no sweep, so it runs on the exclusive
// server: the fingerprint hashes cluster-wide roles another package may
// change on the shared one.
func TestSweep_DataAPIChangeAppliesAtTheNextSweep(t *testing.T) {
	f := exclusiveFixture(t, "an unchanged cluster catalog must run no sweep")
	tbl := f.q("leads")
	f.exec("CREATE TABLE "+tbl+" (id int)", "GRANT SELECT ON "+tbl+" TO "+f.exposed)
	var platform atomic.Pointer[Platform]
	set := func(p Platform) { platform.Store(&p) }
	set(supabasePlatform())
	m := NewMonitor(f.pool, MonitorOptions{Config: func() Config {
		cfg := DefaultConfig()
		cfg.ExposedRoles, cfg.ClientPatterns = []string{f.exposed}, nil
		cfg.Platform = *platform.Load()
		return cfg
	}})
	cat := CategoryPrefix + "AP-03"
	steps := []struct {
		p     Platform
		sev   Severity
		state string
	}{
		{supabasePlatform(), Critical, DataAPIUnknown},
		{supabasePlatform(f.schema), Critical, DataAPIExposed},
		{supabasePlatform("api"), Warning, DataAPINotExposed},
	}
	for _, s := range steps {
		set(s.p)
		run, ran, err := m.Sweep(f.ctx)
		if err != nil || !ran {
			t.Fatalf("%s: sweep ran = %v, err = %v; want a run", s.state, ran, err)
		}
		got := findingOn(run, cat, tbl)
		if got == nil || got.Severity != string(s.sev) ||
			got.Detail["data_api_exposed"] != s.state {
			t.Fatalf("%s: finding = %+v, want %s", s.state, got, s.sev)
		}
		if _, again, err := m.Sweep(f.ctx); err != nil || again {
			t.Fatalf("%s: unchanged state re-ran the sweep (ran %v, err %v)", s.state,
				again, err)
		}
	}
}
