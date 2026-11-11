package action

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// playbooks-06: statistics maintenance is reachable the morning after.
// An operator refreshes a finished investigation's stat_tables evidence
// (one bounded probe, identity checked) and proposes against the new row;
// inconclusive runs qualify; the 15-minute cap, the identity rechecks and
// human approval are unchanged.

func (h *maintenanceHarness) refresh(t *testing.T, evidenceID sre.UUID) sre.Evidence {
	t.Helper()
	ev, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, TableRefresh{EvidenceID: evidenceID,
		Schema: "public", Table: h.table}, "user:1")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return ev
}

func (h *maintenanceHarness) ageEvidence(t *testing.T, id sre.UUID, column string,
	d time.Duration) {
	t.Helper()
	h.rewriteEvidence(t, `UPDATE sage.sre_evidence SET `+column+
		` = clock_timestamp() - make_interval(secs => $2) WHERE id = $1`, string(id), d.Seconds())
}

// rewriteEvidence updates sre_evidence behind its append-only trigger, as
// a superuser could (a tamper, or moving a row's clock for a boundary).
func (h *maintenanceHarness) rewriteEvidence(t *testing.T, sql string, args ...any) {
	t.Helper()
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.ctx) }()
	if _, err := tx.Exec(h.ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	tag, err := tx.Exec(h.ctx, sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("rewrite: %v (%d rows)", err, tag.RowsAffected())
	}
	if err := tx.Commit(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func (h *maintenanceHarness) cite(ev sre.Evidence) StatisticsRequest {
	req := h.req
	req.EvidenceID, req.EvidenceSHA256 = ev.ID, hex.EncodeToString(ev.SHA256)
	return req
}

func concludeInconclusive(h *actionHarness, lease sre.Lease, _ sre.Investigation,
	_ []sre.Evidence) (sre.Investigation, error) {
	return h.st.Conclude(h.ctx, lease, sre.Conclusion{State: sre.StateInconclusive,
		Summary: sre.Summary{Family: "stat_views"}})
}

func stopRun(h *actionHarness, _ sre.Lease, inv sre.Investigation,
	_ []sre.Evidence) (sre.Investigation, error) {
	return h.st.Stop(h.ctx, inv.Scope, inv.ID, inv.Version)
}

func failRun(h *actionHarness, lease sre.Lease, _ sre.Investigation,
	_ []sre.Evidence) (sre.Investigation, error) {
	return h.st.Conclude(h.ctx, lease, sre.Conclusion{State: sre.StateFailed,
		FailureCode: "probe_error"})
}

// State transition: 16-minute-old evidence is refused; after a refresh
// the same request, citing the new row, proposes and queues for a human.
func TestMaintenanceRefresh_StaleEvidenceRefreshedThenProposed(t *testing.T) {
	h := newMaintenanceHarness(t)
	h.ageEvidence(t, h.req.EvidenceID, "observed_at", 16*time.Minute)
	if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1"); err == nil {
		t.Fatal("16-minute-old evidence accepted")
	}
	fresh := h.refresh(t, h.req.EvidenceID)
	if fresh.ID == h.req.EvidenceID || fresh.ProbeID != string(probes.StatTables) ||
		!strings.HasPrefix(fresh.StepKey, sre.RefreshKeyPrefix) || !fresh.VerifyHash() {
		t.Fatalf("refreshed evidence = %+v", fresh)
	}
	newest, err := h.m.NewestTableEvidence(h.ctx, h.inv.ID, "public", h.table)
	if err != nil || newest.ID != fresh.ID {
		t.Fatalf("newest evidence = %s (%v), want the refresh %s", newest.ID, err, fresh.ID)
	}
	p, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.cite(fresh), "user:1")
	if err != nil {
		t.Fatalf("propose on refreshed evidence: %v", err)
	}
	if p.Request.EvidenceID != fresh.ID || p.State != "proposed" ||
		p.ExpiresAt.After(fresh.ObservedAt.Add(15*time.Minute)) {
		t.Fatalf("proposal = %+v", p)
	}
	p, err = h.m.RequestExecution(h.ctx, p.ID, "user:1")
	if err != nil || p.State != "requested" || p.QueueID == 0 || h.exists(t, p) {
		t.Fatalf("request = %+v (%v): must queue for approval and execute nothing", p, err)
	}
	inv, err := h.st.Get(h.ctx, h.inv.Scope, h.inv.ID)
	if err != nil || inv.State != sre.StateConcluded {
		t.Fatalf("investigation after refresh = %s (%v)", inv.State, err)
	}
}

// State transition: an inconclusive run with an operator hypothesis is
// accepted; cancelled and failed runs are still refused.
func TestMaintenanceRefresh_InconclusiveAcceptedOthersRefused(t *testing.T) {
	h := newMaintenanceHarnessWith(t, concludeInconclusive)
	if h.inv.State != sre.StateInconclusive {
		t.Fatalf("harness state = %s", h.inv.State)
	}
	p, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req, "user:1")
	if err != nil || p.State != "proposed" || p.Request.EvidenceID != h.req.EvidenceID {
		t.Fatalf("inconclusive propose = %+v (%v)", p, err)
	}
	if fresh := h.refresh(t, h.req.EvidenceID); fresh.ID == "" {
		t.Fatal("inconclusive refresh wrote nothing")
	}
	for name, finish := range map[string]maintenanceFinish{"cancelled": stopRun,
		"failed": failRun} {
		t.Run(name, func(t *testing.T) {
			h := newMaintenanceHarnessWith(t, finish)
			if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.req,
				"user:1"); !errors.Is(err, ErrProposalState) {
				t.Fatalf("%s propose = %v, want ErrProposalState", name, err)
			}
			_, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, TableRefresh{
				EvidenceID: h.req.EvidenceID, Schema: "public", Table: h.table}, "user:1")
			if !errors.Is(err, ErrProposalState) {
				t.Fatalf("%s refresh = %v, want ErrProposalState", name, err)
			}
		})
	}
}

func TestMaintenanceRefresh_InvalidInputIsRefused(t *testing.T) {
	h := newMaintenanceHarness(t)
	other := h.table + "x"
	if _, err := h.pool.Exec(h.ctx, "CREATE TABLE public."+other+"(a int,b int)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+other) })
	base := TableRefresh{EvidenceID: h.req.EvidenceID, Schema: "public", Table: h.table}
	with := func(fn func(*TableRefresh)) TableRefresh {
		r := base
		fn(&r)
		return r
	}
	for name, tc := range map[string]struct {
		req   TableRefresh
		actor string
	}{
		"table missing from the evidence": {with(func(r *TableRefresh) { r.Table = other }),
			"user:1"},
		"unknown evidence": {with(func(r *TableRefresh) { r.EvidenceID = sre.NewUUID() }),
			"user:1"},
		"malformed id": {with(func(r *TableRefresh) { r.EvidenceID = "x" }), "user:1"},
		"blank schema": {with(func(r *TableRefresh) { r.Schema = "" }), "user:1"},
		"blank table":  {with(func(r *TableRefresh) { r.Table = "" }), "user:1"},
		"blank actor":  {base, ""},
		"injection in name": {with(func(r *TableRefresh) { r.Table += "; DROP TABLE x" }),
			"user:1"},
	} {
		if _, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, tc.req, tc.actor); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := h.m.NewestTableEvidence(h.ctx, h.inv.ID, "public",
		other); !errors.Is(err, sre.ErrNotFound) {
		t.Errorf("newest evidence for an unobserved table = %v, want ErrNotFound", err)
	}
	if _, err := h.pool.Exec(h.ctx, "DROP TABLE public."+h.table+"; CREATE TABLE public."+
		h.table+"(a int,b int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, base, "user:1"); err == nil {
		t.Error("refresh of a recreated table (new OID) accepted")
	}
	all, _ := h.st.Evidence(h.ctx, h.inv.Scope, h.inv.ID)
	if len(all) != 1 {
		t.Fatalf("refused refreshes wrote %d evidence rows", len(all)-1)
	}
}

func TestMaintenanceRefresh_NonStatTablesAndPurgedRefused(t *testing.T) {
	t.Run("non-stat_tables evidence", func(t *testing.T) {
		h := newMaintenanceHarness(t, func(res *probes.Result) {
			res.ProbeID = probes.StatDatabase
		})
		if _, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, TableRefresh{
			EvidenceID: h.req.EvidenceID, Schema: "public", Table: h.table},
			"user:1"); !errors.Is(err, sre.ErrInvalidRequest) {
			t.Fatalf("refresh of stat_database evidence = %v, want ErrInvalidRequest", err)
		}
	})
	t.Run("purged evidence", func(t *testing.T) {
		h := newMaintenanceHarness(t)
		if _, err := h.pool.Exec(h.ctx, `UPDATE sage.sre_investigations
			SET evidence_purged_at = clock_timestamp() WHERE id = $1`,
			string(h.inv.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, TableRefresh{
			EvidenceID: h.req.EvidenceID, Schema: "public", Table: h.table},
			"user:1"); !errors.Is(err, ErrProposalState) {
			t.Fatalf("refresh of purged evidence = %v, want ErrProposalState", err)
		}
	})
}

// Integrity: a rewritten refresh row fails its hash and the proposal; an
// unattested refresh row breaks the chain and blocks every proposal.
func TestMaintenanceRefresh_TamperingIsRefused(t *testing.T) {
	h := newMaintenanceHarness(t)
	fresh := h.refresh(t, h.req.EvidenceID)
	h.rewriteEvidence(t, `UPDATE sage.sre_evidence
		SET payload = jsonb_set(payload, '{status}', '"ok "') WHERE id = $1`, string(fresh.ID))
	tampered, err := h.st.EvidenceByID(h.ctx, h.inv.Scope, h.inv.ID, fresh.ID)
	if err != nil || tampered.VerifyHash() {
		t.Fatalf("tampered refresh verifies (%v)", err)
	}
	if _, err := h.m.ProposeStatistics(h.ctx, h.inv.ID, h.cite(fresh), "user:1"); err == nil {
		t.Fatal("tampered refresh evidence accepted")
	}
	g := newMaintenanceHarness(t)
	clean := g.refresh(t, g.req.EvidenceID)
	if _, err := g.pool.Exec(g.ctx, `INSERT INTO sage.sre_evidence
		(deployment_id, database_id, investigation_id, id, step_id, source_kind, probe_id,
		 probe_version, observed_at, capability_state, payload_version, classification,
		 payload, sha256)
		SELECT deployment_id, database_id, investigation_id, gen_random_uuid(), step_id,
		       source_kind, probe_id, probe_version, clock_timestamp(), capability_state,
		       payload_version, classification, payload, sha256
		FROM sage.sre_evidence WHERE id = $1`, string(clean.ID)); err != nil {
		t.Fatal(err)
	}
	if err := g.st.VerifyEvents(g.ctx, g.inv.Scope, g.inv.ID); !errors.Is(err, sre.ErrChainBroken) {
		t.Fatalf("out-of-band refresh row = %v, want ErrChainBroken", err)
	}
	if _, err := g.m.ProposeStatistics(g.ctx, g.inv.ID, g.cite(clean), "user:1"); err == nil {
		t.Fatal("proposal accepted on a broken chain")
	}
}

// Boundary: one refresh per investigation per minute (database clock).
func TestMaintenanceRefresh_RateLimitBoundary(t *testing.T) {
	h := newMaintenanceHarness(t)
	first := h.refresh(t, h.req.EvidenceID)
	req := TableRefresh{EvidenceID: h.req.EvidenceID, Schema: "public", Table: h.table}
	if _, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, req,
		"user:1"); !errors.Is(err, sre.ErrRefreshTooSoon) {
		t.Fatalf("immediate refresh = %v, want ErrRefreshTooSoon", err)
	}
	h.ageEvidence(t, first.ID, "collected_at", 59*time.Second)
	if _, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, req,
		"user:1"); !errors.Is(err, sre.ErrRefreshTooSoon) {
		t.Fatalf("refresh at 59 s = %v, want ErrRefreshTooSoon", err)
	}
	h.ageEvidence(t, first.ID, "collected_at", 60*time.Second)
	if _, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, req, "user:1"); err != nil {
		t.Fatalf("refresh at 60 s: %v", err)
	}
	if RefreshInterval != time.Minute {
		t.Fatalf("RefreshInterval = %s, want 1m", RefreshInterval)
	}
}

// Boundary: evidence aged exactly MaxEvidenceAge is fresh; 1 s more is not.
// No database: the age comparison is a pure function of its inputs.
func TestEvidenceFresh_Boundary(t *testing.T) {
	limit := 15 * time.Minute
	for age, want := range map[time.Duration]bool{
		0: true, limit: true, limit + time.Second: false, limit + time.Nanosecond: false,
		-time.Second: true, -time.Second - time.Nanosecond: false,
	} {
		if got := evidenceFresh(age, limit); got != want {
			t.Errorf("evidenceFresh(%s, %s) = %v, want %v", age, limit, got, want)
		}
	}
}

// Concurrency: two refreshes at once; the rate limit holds under the
// investigation lock, so exactly one lands and the chain stays valid.
func TestMaintenanceRefresh_ConcurrentRefreshesOneLands(t *testing.T) {
	h := newMaintenanceHarness(t)
	req := TableRefresh{EvidenceID: h.req.EvidenceID, Schema: "public", Table: h.table}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = h.m.RefreshTableEvidence(context.WithoutCancel(h.ctx), h.inv.ID,
				req, "user:1")
		}(i)
	}
	wg.Wait()
	ok, limited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, sre.ErrRefreshTooSoon):
			limited++
		default:
			t.Fatalf("concurrent refresh: %v", err)
		}
	}
	if ok != 1 || limited != 1 {
		t.Fatalf("concurrent refreshes: %d landed, %d limited", ok, limited)
	}
	if err := h.st.VerifyEvents(h.ctx, h.inv.Scope, h.inv.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// Rollout: the investigation timeline renders a refresh step whose next
// state is the terminal state, with a verified hash and chain.
func TestMaintenanceRefresh_TimelineRendersTheRefreshStep(t *testing.T) {
	h := newMaintenanceHarness(t)
	fresh := h.refresh(t, h.req.EvidenceID)
	d, err := h.svc.Detail(h.ctx, h.inv.ID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d.Investigation.State != sre.StateConcluded || !d.ChainVerified || len(d.Evidence) != 2 {
		t.Fatalf("detail = state %s, chain %v, %d evidence", d.Investigation.State,
			d.ChainVerified, len(d.Evidence))
	}
	got := d.Evidence[1]
	if got.ID != fresh.ID || !strings.HasPrefix(got.Step, sre.RefreshKeyPrefix) ||
		!got.HashVerified || got.ProbeID != string(probes.StatTables) {
		t.Fatalf("refresh evidence view = %+v", got)
	}
}

// The MCP path: the server, not the caller, resolves the newest retained
// stat_tables evidence naming the table, refreshes it, and cites it.
func TestMaintenanceRefresh_TableCallsResolveTheNewestEvidence(t *testing.T) {
	h := newMaintenanceHarness(t)
	h.ageEvidence(t, h.req.EvidenceID, "observed_at", 16*time.Minute)
	ask := TableStatistics{Schema: "public", Table: h.table, Columns: h.req.Columns,
		Rationale: h.req.Rationale}
	if _, err := h.m.ProposeStatisticsForTable(h.ctx, h.inv.ID, ask, "user:1"); err == nil {
		t.Fatal("stale newest evidence accepted")
	}
	fresh, err := h.m.RefreshTable(h.ctx, h.inv.ID, "public", h.table, "user:1")
	if err != nil || fresh.ID == h.req.EvidenceID {
		t.Fatalf("table refresh = %+v (%v)", fresh, err)
	}
	p, err := h.m.ProposeStatisticsForTable(h.ctx, h.inv.ID, ask, "user:1")
	if err != nil {
		t.Fatalf("table propose: %v", err)
	}
	if p.Request.EvidenceID != fresh.ID ||
		p.Request.EvidenceSHA256 != hex.EncodeToString(fresh.SHA256) ||
		strings.Join(p.Request.Kinds, ",") != "dependencies,ndistinct" {
		t.Fatalf("table proposal cites %s/%s kinds %v, want the refresh %s",
			p.Request.EvidenceID, p.Request.EvidenceSHA256, p.Request.Kinds, fresh.ID)
	}
	if _, err := h.m.RefreshTable(h.ctx, h.inv.ID, "public", h.table+"_none",
		"user:1"); !errors.Is(err, sre.ErrNotFound) {
		t.Fatalf("refresh of an unobserved table = %v, want ErrNotFound", err)
	}
	ask.Columns = []string{"a", "c"}
	if _, err := h.m.ProposeStatisticsForTable(h.ctx, h.inv.ID, ask,
		"user:1"); !errors.Is(err, ErrProposalState) && !errors.Is(err, sre.ErrInvalidRequest) {
		t.Fatalf("second, different proposal = %v, want a refusal", err)
	}
}

// The rate limit is checked before the probe: a refresh inside the minute
// is refused as too soon even when the table was since recreated, which
// the probe would have reported, and writes nothing.
func TestMaintenanceRefresh_RateLimitPrecedesTheProbe(t *testing.T) {
	h := newMaintenanceHarness(t)
	h.refresh(t, h.req.EvidenceID)
	if _, err := h.pool.Exec(h.ctx, "DROP TABLE public."+h.table+"; CREATE TABLE public."+
		h.table+"(a int,b int)"); err != nil {
		t.Fatal(err)
	}
	req := TableRefresh{EvidenceID: h.req.EvidenceID, Schema: "public", Table: h.table}
	_, err := h.m.RefreshTableEvidence(h.ctx, h.inv.ID, req, "user:1")
	if !errors.Is(err, sre.ErrRefreshTooSoon) || errors.Is(err, sre.ErrInvalidRequest) {
		t.Fatalf("refresh inside the minute = %v, want only ErrRefreshTooSoon", err)
	}
	all, err := h.st.Evidence(h.ctx, h.inv.Scope, h.inv.ID)
	if err != nil || len(all) != 2 {
		t.Fatalf("evidence after a limited refresh = %d rows (%v), want 2", len(all), err)
	}
}
