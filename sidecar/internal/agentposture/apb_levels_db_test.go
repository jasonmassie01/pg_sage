package agentposture

import "testing"

// G0-05 for AP-09..AP-16: one fixture where each of them finds something,
// and every finding holds to the L1 contract (requireL1). The registry-
// wide G0-05 test covers them too; this one proves none is vacuous.
func TestG005_PostureBDetectorsProposeOnlyL1(t *testing.T) {
	f := newFixture(t)
	agent := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")
	hint := sharedLogin(f, "claude-code", "billing", "reports")
	seedCheckpoints(f)
	f.exec("CREATE EXTENSION dblink SCHEMA "+f.schema,
		"CREATE TABLE "+f.q("emb")+" (v public.vector(2))",
		"CREATE INDEX ON "+f.q("emb")+" USING hnsw (v public.vector_l2_ops)",
		"CREATE TABLE "+f.q("pub_t")+" (id int)",
		"GRANT SELECT ON "+f.q("pub_t")+" TO PUBLIC")
	store := NewObservationStore()
	env := f.env(func(e *Env) {
		withClients(e)
		f.agent(e, agent, SourceRegistered)
		f.agent(e, hint, SourceClientHint)
		e.Observations = store
		e.Config.MemoryGrowthGBDay = 1e-6
	})
	f.run("AP-12", env) // the first observation
	backdate(t, store)
	f.exec("INSERT INTO " + f.q("checkpoints") +
		" SELECT g::text, md5(g::text) FROM generate_series(1, 3000) g")

	for _, id := range []string{"AP-09", "AP-10", "AP-11", "AP-12", "AP-13", "AP-14",
		"AP-15", "AP-16"} {
		o := f.run(id, env)
		if len(o.Findings) == 0 {
			t.Errorf("%s found nothing in the combined fixture: the L1 check is vacuous", id)
		}
		for _, fd := range o.Findings {
			requireL1(t, fd)
		}
	}
}

// Every AP-09..AP-16 detector is registered with the severity the spec's
// table gives as its highest.
func TestDefaultRegistry_ShipsAP09ToAP16(t *testing.T) {
	want := map[string]Severity{"AP-09": Critical, "AP-10": Critical, "AP-11": Warning,
		"AP-12": Info, "AP-13": Warning, "AP-14": Warning, "AP-15": Warning,
		"AP-16": Warning}
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
}
