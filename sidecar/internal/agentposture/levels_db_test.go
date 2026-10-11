package agentposture

import (
	"strings"
	"testing"
)

// seedEveryViolation gives AP-01..AP-08 something to find.
func seedEveryViolation(f *fixture) (agent string) {
	agent = registeredRoleName(f.t)
	createRole(f.t, f.ctx, f.pool, agent, "LOGIN BYPASSRLS")
	f.exec(
		"CREATE TABLE "+f.q("open_t")+" (id int)",
		"GRANT SELECT ON "+f.q("open_t")+" TO "+f.exposed,
		"ALTER TABLE "+f.q("open_t")+" OWNER TO "+agent,
		"CREATE TABLE "+f.q("rls_t")+" (id int)",
		"ALTER TABLE "+f.q("rls_t")+" ENABLE ROW LEVEL SECURITY",
		"CREATE POLICY all_rows ON "+f.q("rls_t")+" TO "+f.exposed+" USING (true)",
		"CREATE VIEW "+f.q("rls_v")+" AS SELECT id FROM "+f.q("rls_t"),
		"CREATE FUNCTION "+f.q("def_fn")+"() RETURNS int LANGUAGE sql SECURITY DEFINER "+
			"AS 'SELECT 1'",
		"GRANT CREATE ON SCHEMA "+f.schema+" TO PUBLIC",
	)
	return agent
}

// G0-05: no posture finding proposes above L1. The test runs every
// detector of the default registry, including those added after it was
// written, and holds each finding to the L1 contract: the fix is a manual
// script, and neither the analyzer finding (which the executor would act
// on through RecommendedSQL) nor the first-look item carries more.
func TestG005_NoPostureFindingProposesAboveL1(t *testing.T) {
	f := newFixture(t)
	agent := seedEveryViolation(f)
	env := f.env(func(e *Env) { f.agent(e, agent, SourceRegistered) })
	fired := map[string]int{}
	for _, d := range Default().Detectors() {
		o := f.run(d.Spec().ID, env)
		for _, fd := range o.Findings {
			fired[o.Detector]++
			requireL1(t, fd)
		}
	}
	for _, id := range []string{"AP-01", "AP-02", "AP-03", "AP-04", "AP-05", "AP-06",
		"AP-07", "AP-08"} {
		if fired[id] == 0 {
			t.Errorf("%s found nothing in the combined fixture: the L1 check is vacuous", id)
		}
	}
}

func requireL1(t *testing.T, fd Finding) {
	t.Helper()
	a := fd.AnalyzerFinding()
	if strings.TrimSpace(a.RecommendedSQL) != "" || a.RollbackSQL != "" || a.ActionRisk != "" {
		t.Fatalf("%s on %s proposes executable SQL: %q (risk %q)", fd.Detector, fd.Object,
			a.RecommendedSQL, a.ActionRisk)
	}
	if a.Detail["proposal_level"] != "L1" || a.Detail["manual_script"] != fd.FixScript {
		t.Fatalf("%s on %s: detail %v, want proposal_level L1 and the manual script",
			fd.Detector, fd.Object, a.Detail)
	}
	if !IsCategory(a.Category) || a.RuleID != fd.Detector {
		t.Fatalf("%s on %s: category %q rule %q", fd.Detector, fd.Object, a.Category,
			a.RuleID)
	}
}

// Every shipped detector declares a valid spec in the default registry.
func TestDefaultRegistry_ShipsAP01ToAP08(t *testing.T) {
	want := map[string]Severity{"AP-01": Critical, "AP-02": Critical, "AP-03": Critical,
		"AP-04": Warning, "AP-05": Warning, "AP-06": Warning, "AP-07": Warning,
		"AP-08": Info}
	for id, sev := range want {
		d, ok := Default().Get(id)
		if !ok {
			t.Errorf("%s is not registered", id)
			continue
		}
		if s := d.Spec(); s.Severity != sev || s.Title == "" {
			t.Errorf("%s spec = %+v, want severity %s", id, s, sev)
		}
	}
	d, _ := Default().Get("AP-06")
	if d == nil || len(d.Spec().Arms) != 2 {
		t.Fatal("AP-06 must declare its PG15+ and PG14 arms")
	}
}
