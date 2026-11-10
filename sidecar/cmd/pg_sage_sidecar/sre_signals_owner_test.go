package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// playbooks-08: in a fleet, an app SLO with no database is evaluated by
// every database's engine. Only the engine of its owner_database opens
// the slo_burn investigation; the others log that the burn is owned
// elsewhere. Idempotency keys and the per-database evaluation (autonomy
// caps) are unchanged.

// logRecorder captures the signals' log lines.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) log(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(format, args...))
}

func (l *logRecorder) has(level, text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.HasPrefix(line, level+" ") && strings.Contains(line, text) {
			return true
		}
	}
	return false
}

func ownedSettings(name, owner string) config.SREConfig {
	s := sloSettings()
	s.SLO.Objectives = []config.SREObjectiveConfig{{Name: name, Source: "push",
		Target: 0.995, OwnerDatabase: owner}}
	return s
}

// uniqueSLO is an SLO name no other test run on the server shares.
func uniqueSLO() string { return "payments-" + string(sre.NewUUID())[:8] }

func TestSLOObjectivesOwnerDatabase(t *testing.T) {
	s := ownedSettings("payments", "billing")
	for _, db := range []string{"orders", "billing"} {
		objs, err := sloObjectives(s.SLO, db)
		if err != nil || len(objs) != 1 {
			t.Fatalf("%s objectives = %+v err=%v", db, objs, err)
		}
		o := objs[0]
		if o.Database != db || o.OwnerDatabase != "billing" || o.Name != "payments" {
			t.Fatalf("%s objective = %+v", db, o)
		}
	}
	bare := sloSettings()
	objs, err := sloObjectives(bare.SLO, "orders")
	if err != nil || len(objs) != 2 || objs[0].OwnerDatabase != "" ||
		objs[1].OwnerDatabase != "" {
		t.Fatalf("objectives without an owner = %+v err=%v", objs, err)
	}
}

func TestMissingSLOOwners(t *testing.T) {
	s := sloSettings()
	s.SLO.Objectives = append(s.SLO.Objectives,
		config.SREObjectiveConfig{Name: "ledger", Source: "push", Target: 0.99,
			OwnerDatabase: "billing"},
		config.SREObjectiveConfig{Name: "search", Source: "push", Target: 0.99,
			OwnerDatabase: "inventory"})
	got := missingSLOOwners(s.SLO, []string{"orders", "billing"})
	if len(got) != 1 || !strings.Contains(got[0], "search") ||
		!strings.Contains(got[0], `"inventory"`) {
		t.Fatalf("missing owners = %q, want only search/inventory", got)
	}
	if got := missingSLOOwners(s.SLO, nil); len(got) != 2 {
		t.Fatalf("no discovered databases: missing = %q, want ledger and search", got)
	}
	if got := missingSLOOwners(sloSettings().SLO, nil); len(got) != 0 {
		t.Fatalf("objectives without owners warned: %q", got)
	}
	if got := missingSLOOwners(config.SRESLOConfig{}, []string{"orders"}); len(got) != 0 {
		t.Fatalf("no objectives warned: %q", got)
	}
	var rec logRecorder
	warnMissingSLOOwners(s.SLO, []string{"orders"}, rec.log)
	if !rec.has("WARN", `"inventory"`) || !rec.has("WARN", `"billing"`) {
		t.Fatalf("warnings = %q", rec.lines)
	}
}

// burnRows counts the slo_burn investigations of one SLO across every
// database (CHECK-14).
func burnRows(t *testing.T, r signalsRig, name string) int {
	t.Helper()
	var n int
	err := r.pool.QueryRow(r.ctx, `SELECT count(*) FROM sage.sre_investigations
		WHERE trigger_kind = 'slo_burn' AND source_case_id = $1`, "slo:"+name).Scan(&n)
	if err != nil {
		t.Fatalf("count slo_burn rows: %v", err)
	}
	return n
}

func TestSRESignals_OwnerDatabaseOpensOneInvestigation(t *testing.T) {
	name := uniqueSLO()
	settings := ownedSettings(name, "billing")
	orders := signalsFixtureNamed(t, settings, "orders")
	billing := signalsFixtureNamed(t, settings, "billing")
	var rec logRecorder
	orders.sig.logFn = rec.log
	st := pageStatus(name, slo.KindApp)
	for _, r := range []signalsRig{orders, billing} {
		if err := r.sig.onPage(r.ctx, st); err != nil {
			t.Fatalf("onPage %s: %v", r.sig.name, err)
		}
	}
	if n := burnRows(t, orders, name); n != 1 {
		t.Fatalf("one fleet burn opened %d slo_burn investigations, want 1", n)
	}
	if n := burnInvestigations(t, billing); n != 1 {
		t.Fatalf("owner billing opened %d investigations, want 1", n)
	}
	if n := burnInvestigations(t, orders); n != 0 {
		t.Fatalf("non-owner orders opened %d investigations, want 0", n)
	}
	if !rec.has("INFO", "burn owned by billing") {
		t.Fatalf("orders did not log the owner: %q", rec.lines)
	}
}

func TestSRESignals_MissingOwnerOpensNone(t *testing.T) {
	name := uniqueSLO()
	settings := ownedSettings(name, "inventory")
	orders := signalsFixtureNamed(t, settings, "orders")
	billing := signalsFixtureNamed(t, settings, "billing")
	st := pageStatus(name, slo.KindApp)
	for _, r := range []signalsRig{orders, billing} {
		if err := r.sig.onPage(r.ctx, st); err != nil {
			t.Fatalf("onPage %s: %v", r.sig.name, err)
		}
	}
	if n := burnRows(t, orders, name); n != 0 {
		t.Fatalf("an owner outside the fleet opened %d investigations", n)
	}
	if got := missingSLOOwners(settings.SLO, []string{"orders", "billing"}); len(got) != 1 {
		t.Fatalf("missing owner not reported: %q", got)
	}
}

func TestSRESignals_ConcurrentPagesOpenOne(t *testing.T) {
	name := uniqueSLO()
	settings := ownedSettings(name, "orders")
	rigs := []signalsRig{signalsFixtureNamed(t, settings, "orders"),
		signalsFixtureNamed(t, settings, "billing")}
	st := pageStatus(name, slo.KindApp)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		for _, r := range rigs {
			wg.Add(1)
			go func(r signalsRig) {
				defer wg.Done()
				errs <- r.sig.onPage(r.ctx, st)
			}(r)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent onPage: %v", err)
		}
	}
	if n := burnRows(t, rigs[0], name); n != 1 {
		t.Fatalf("concurrent pages opened %d slo_burn investigations, want 1", n)
	}
	if n := burnInvestigations(t, rigs[0]); n != 1 {
		t.Fatalf("owner orders opened %d investigations, want 1", n)
	}
}

// Unchanged: without owner_database every database still opens its own
// (standalone has one; a bound objective reaches one database).
func TestSRESignals_NoOwnerKeepsPerDatabasePage(t *testing.T) {
	name := uniqueSLO()
	settings := ownedSettings(name, "")
	r := signalsFixtureNamed(t, settings, "orders")
	if err := r.sig.onPage(r.ctx, pageStatus(name, slo.KindApp)); err != nil {
		t.Fatal(err)
	}
	if n := burnRows(t, r, name); n != 1 {
		t.Fatalf("an objective without an owner opened %d, want 1", n)
	}
	var key string
	err := r.pool.QueryRow(r.ctx, `SELECT idempotency_key FROM sage.sre_investigations
		WHERE source_case_id = $1`, "slo:"+name).Scan(&key)
	if err != nil || !strings.HasPrefix(key, "slo:"+name+":") {
		t.Fatalf("idempotency key = %q err=%v", key, err)
	}
}

func TestUnevaluatedOwnedSLOs(t *testing.T) {
	cfg := ownedSettings("payments", "billing").SLO
	cfg.Objectives = append(cfg.Objectives, config.SREObjectiveConfig{Name: "ledger",
		Source: "push", Target: 0.99, OwnerDatabase: "orders"})
	evaluated := map[string]string{"payments": "billing", "ledger": "orders"}
	if got := unevaluatedOwnedSLOs(cfg, "billing", evaluated); len(got) != 0 {
		t.Fatalf("an evaluated owned SLO warned: %q", got)
	}
	if got := unevaluatedOwnedSLOs(cfg, "billing", nil); len(got) != 1 ||
		got[0] != "payments" {
		t.Fatalf("owner without an engine: %q, want [payments]", got)
	}
	if got := unevaluatedOwnedSLOs(cfg, "inventory", nil); len(got) != 0 {
		t.Fatalf("a database that owns nothing warned: %q", got)
	}
	if got := unevaluatedOwnedSLOs(config.SRESLOConfig{}, "billing", nil); len(got) != 0 {
		t.Fatalf("no objectives warned: %q", got)
	}
}

// An owner whose read policy drops the owned objective (database scope
// keeps only bound push objectives; a narrow allowlist builds no engine)
// would silently swallow every burn: the others defer to it. It warns.
func TestSRESignals_OwnerThatCannotEvaluateWarns(t *testing.T) {
	name := uniqueSLO()
	settings := ownedSettings(name, "billing")
	r := signalsFixtureNamed(t, settings, "billing")
	database := r.pool.Config().ConnConfig.Database
	scoped := &probes.ReadPolicy{Version: 1, Scope: "database", Database: database,
		Binding: probes.PoolBinding(r.pool)}
	narrow := &probes.ReadPolicy{Version: 1, Scope: "cluster", Database: database,
		Binding: probes.PoolBinding(r.pool), Allowed: []probes.ID{probes.StatTables}}
	cases := []struct {
		label  string
		policy *probes.ReadPolicy
		db     string
		warn   bool
	}{{"database scope", scoped, "billing", true}, {"no engine", narrow, "billing", true},
		{"full policy", nil, "billing", false}, {"not the owner", scoped, "orders", false}}
	for _, c := range cases {
		var rec logRecorder
		sig, err := newSRESignals(sreSignalsDeps{control: r.pool, monitored: r.pool,
			name: c.db, readPolicy: c.policy, settings: settings, logFn: rec.log})
		if err != nil {
			t.Fatalf("%s: newSRESignals: %v", c.label, err)
		}
		if got := rec.has("WARN", name+" names billing as owner_database"); got != c.warn {
			t.Fatalf("%s: warned=%v, want %v: %q", c.label, got, c.warn, rec.lines)
		}
		if c.label == "full policy" && sig.owners[name] != "billing" {
			t.Fatalf("full policy did not evaluate the owned SLO: %v", sig.owners)
		}
	}
	off := settings
	off.SLO.Enabled = false
	var rec logRecorder
	if _, err := newSRESignals(sreSignalsDeps{control: r.pool, monitored: r.pool,
		name: "billing", readPolicy: narrow, settings: off, logFn: rec.log}); err != nil ||
		rec.has("WARN", "owner_database") {
		t.Fatalf("SLO layer off: err=%v lines=%q", err, rec.lines)
	}
}
