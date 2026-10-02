package causal

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Wraparound runway / autovacuum starvation (AI-SRE-SPEC §4 R2): what
// keeps a table's freeze horizon from advancing while it nears (or has
// passed) its freeze maximum: a session, prepared transaction or
// replication slot holding the xmin horizon, busy, disabled or cancelled
// autovacuum, or a surge in XID consumption. Nothing is blamed unless a
// table is near its maximum or the measured runway is short.

type wtable struct {
	rel      string
	xid, max float64
	enabled  bool
}

type wholder struct {
	kind, name, state string
	pid               int64
	age               float64
}

func xidSample(id string, at time.Time, next float64, workers, maxW int64,
	avOn bool) Observation {
	o := obs(id, probes.XIDRunwayProbe, probes.Row{"next_xid": next,
		"mxid_counter": 10.0, "cluster_xid_age": 150000.0, "cluster_mxid_age": 5.0,
		"database_xid_age": 150000.0, "oldest_database": "app",
		"freeze_max_age": int64(200000000), "multixact_freeze_max_age": int64(400000000),
		"autovacuum_on": avOn, "autovacuum_max_workers": maxW,
		"autovacuum_workers": workers})
	o.Result.ObservedAt = at
	return o
}

func tablesObs(id string, ts ...wtable) Observation {
	var rows []probes.Row
	for _, t := range ts {
		rows = append(rows, probes.Row{"relation": t.rel, "xid_age": t.xid,
			"mxid_age": 1.0, "freeze_max_age": t.max, "mxid_freeze_max_age": 4e8,
			"autovacuum_enabled": t.enabled})
	}
	return obs(id, probes.WraparoundTablesProbe, rows...)
}

func holdersObs(id string, hs ...wholder) Observation {
	var rows []probes.Row
	for _, h := range hs {
		row := probes.Row{"holder_kind": h.kind, "state": h.state, "xmin_age": h.age}
		if h.pid > 0 {
			row["pid"] = h.pid
		}
		if h.name != "" {
			row["holder_name"] = h.name
		}
		rows = append(rows, row)
	}
	return obs(id, probes.XminHorizon, rows...)
}

func cancelObs(id string, n int64) Observation {
	return obs(id, probes.AutovacuumCancellations, probes.Row{"cancel_incidents": n})
}

func trendRow(kind, subject string, n int64, last, limit, rate, r2 float64) probes.Row {
	row := probes.Row{"kind": kind, "subject": subject, "samples": n,
		"first_at": t0.Add(-time.Hour), "last_at": t0, "last_value": last,
		"rate_per_s": rate, "r2": r2}
	if !math.IsNaN(limit) {
		row["last_limit"] = limit
	}
	return row
}

func trendsObs(id string, rows ...probes.Row) Observation {
	return obs(id, probes.RunwayTrendsProbe, rows...)
}

var overdue = wtable{rel: "public.events", xid: 150000, max: 100000, enabled: true}

// wrapCase is the default healthy-autovacuum case around one table and
// one set of holders: 1 of 3 workers, no cancellations, a steady XID rate
// of 5/s in both the trend and the 5 s sample window.
func wrapCase(table wtable, holders ...wholder) []Observation {
	return []Observation{
		xidSample("E1", t0, 1e6, 1, 3, true),
		tablesObs("E2", table, wtable{rel: "public.users", xid: 1000, max: 2e8,
			enabled: true}),
		holdersObs("E3", holders...),
		cancelObs("E4", 0),
		trendsObs("E5", trendRow(probes.RunwayXID, probes.SubjectCluster, 61,
			150000, 2107483647, 5, 1)),
		xidSample("E6", t0.Add(5*time.Second), 1e6+25, 1, 3, true),
	}
}

func TestWraparound_SessionHolderPinsAnOverdueTable(t *testing.T) {
	d := DiagnoseWraparound(wrapCase(overdue, wholder{kind: probes.HolderSession,
		pid: 4242, state: "idle in transaction", age: 149000},
		wholder{kind: probes.HolderSession, pid: 7, state: "active", age: 3}), "xid")
	if d.Family != FamilyWraparound || d.GraphVersion != GraphVersion {
		t.Fatalf("family/version = %s/%s", d.Family, d.GraphVersion)
	}
	root := requireStatus(t, d, XminHeldBySession, StatusRoot)
	if root.Subject != "pid 4242" || !(root.Confidence >= 0.7) ||
		root.Support[0].EvidenceID != "E3" ||
		!strings.Contains(root.Support[0].Text, "149000") ||
		!strings.Contains(root.Support[0].Text, "idle in transaction") {
		t.Fatalf("session root = %+v", root)
	}
	prepared := requireStatus(t, d, XminHeldByPreparedXact, StatusRuledOut)
	if !strings.Contains(prepared.Contradict[0].Text, "no prepared transaction") {
		t.Fatalf("prepared contradiction = %+v", prepared.Contradict)
	}
	requireStatus(t, d, XminHeldByReplication, StatusRuledOut)
	sat := requireStatus(t, d, AutovacuumSaturated, StatusRuledOut)
	if !strings.Contains(sat.Contradict[0].Text, "1 of 3") {
		t.Fatalf("saturation contradiction = %+v", sat.Contradict)
	}
	requireStatus(t, d, AutovacuumDisabled, StatusRuledOut)
	requireStatus(t, d, AutovacuumCancelled, StatusAlternative)
	requireStatus(t, d, XIDConsumptionSurge, StatusRuledOut)
	if !hasObserved(d, "E2", "public.events", "150000", "100000") {
		t.Fatalf("observed = %+v, want the table's age against its maximum", d.Observed)
	}
}

func TestWraparound_PreparedTransactionHolder(t *testing.T) {
	d := DiagnoseWraparound(wrapCase(overdue,
		wholder{kind: probes.HolderPreparedXact, name: "9f1c", state: "prepared",
			age: 149000},
		wholder{kind: probes.HolderSession, pid: 7, state: "active", age: 100}), "xid")
	root := requireStatus(t, d, XminHeldByPreparedXact, StatusRoot)
	if root.Subject != "prepared transaction 9f1c" {
		t.Fatalf("prepared root = %+v", root)
	}
	session := requireStatus(t, d, XminHeldBySession, StatusRuledOut)
	if !strings.Contains(session.Contradict[0].Text, "pid 7") {
		t.Fatalf("young session contradiction = %+v", session.Contradict)
	}
}

func TestWraparound_ReplicationSlotHolder(t *testing.T) {
	d := DiagnoseWraparound(wrapCase(overdue,
		wholder{kind: probes.HolderSlotCatalog, name: "cdc", state: "inactive",
			age: 140000}), "xid")
	root := requireStatus(t, d, XminHeldByReplication, StatusRoot)
	if root.Subject != `slot "cdc"` {
		t.Fatalf("slot root = %+v", root)
	}
	requireStatus(t, d, XminHeldBySession, StatusRuledOut)
}

// Boundary: a holder at exactly half the table's XID age pins it; just
// under half it does not (and is not refuted above 10%).
func TestWraparound_PinningBoundary(t *testing.T) {
	d := DiagnoseWraparound(wrapCase(overdue, wholder{kind: probes.HolderSession,
		pid: 1, state: "active", age: 75000}), "xid")
	requireStatus(t, d, XminHeldBySession, StatusRoot)
	d = DiagnoseWraparound(wrapCase(overdue, wholder{kind: probes.HolderSession,
		pid: 1, state: "active", age: math.Nextafter(75000, 0)}), "xid")
	requireStatus(t, d, XminHeldBySession, StatusAlternative)
	d = DiagnoseWraparound(wrapCase(overdue, wholder{kind: probes.HolderSession,
		pid: 1, state: "active", age: 14999}), "xid")
	requireStatus(t, d, XminHeldBySession, StatusRuledOut)
}

// A table far from its maximum is not an incident even when a long
// transaction pins it: no root (benign lookalike).
func TestWraparound_NotNearIsInconclusive(t *testing.T) {
	far := wtable{rel: "public.events", xid: 2000, max: 2e8, enabled: true}
	d := DiagnoseWraparound(wrapCase(far, wholder{kind: probes.HolderSession, pid: 9,
		state: "idle in transaction", age: 1990}), "xid")
	if d.Conclusive || d.Root != nil {
		t.Fatalf("far table produced root %+v", d.Root)
	}
	h := requireStatus(t, d, XminHeldBySession, StatusAlternative)
	if h.Confidence >= SupportThreshold {
		t.Fatalf("not-near holder confidence %v", h.Confidence)
	}
	if !strings.Contains(d.Reason, "near") {
		t.Fatalf("reason = %q, want it to say nothing is near its limit", d.Reason)
	}
}

// Boundary: exactly half the maximum is near.
func TestWraparound_NearBoundary(t *testing.T) {
	half := wtable{rel: "public.events", xid: 100000, max: 200000, enabled: true}
	d := DiagnoseWraparound(wrapCase(half, wholder{kind: probes.HolderSession, pid: 9,
		state: "active", age: 99000}), "xid")
	requireStatus(t, d, XminHeldBySession, StatusRoot)
	below := wtable{rel: "public.events", xid: math.Nextafter(100000, 0), max: 200000,
		enabled: true}
	d = DiagnoseWraparound(wrapCase(below, wholder{kind: probes.HolderSession, pid: 9,
		state: "active", age: 99000}), "xid")
	if d.Root != nil {
		t.Fatalf("just under half the maximum produced root %+v", d.Root)
	}
}

// Decoy: an idle-in-transaction session that holds no old horizon is
// not blamed for a table nearing its maximum.
func TestWraparound_IdleSessionWithoutHorizonIsRuledOut(t *testing.T) {
	near := wtable{rel: "public.events", xid: 90000, max: 100000, enabled: true}
	d := DiagnoseWraparound(wrapCase(near, wholder{kind: probes.HolderSession, pid: 31,
		state: "idle in transaction", age: 5}), "xid")
	if d.Root != nil {
		t.Fatalf("decoy produced root %+v", d.Root)
	}
	h := requireStatus(t, d, XminHeldBySession, StatusRuledOut)
	if !strings.Contains(h.Contradict[0].Text, "age 5") {
		t.Fatalf("contradiction = %+v", h.Contradict)
	}
}

func TestWraparound_AutovacuumSaturated(t *testing.T) {
	obs := wrapCase(overdue)
	obs[0] = xidSample("E1", t0, 1e6, 3, 3, true)
	obs[5] = xidSample("E6", t0.Add(5*time.Second), 1e6+25, 3, 3, true)
	d := DiagnoseWraparound(obs, "xid")
	root := requireStatus(t, d, AutovacuumSaturated, StatusRoot)
	if !strings.Contains(root.Support[0].Text, "3 of 3") || root.Subject != "autovacuum" {
		t.Fatalf("saturated root = %+v", root)
	}
	for _, id := range []NodeID{XminHeldBySession, XminHeldByPreparedXact,
		XminHeldByReplication} {
		h := requireStatus(t, d, id, StatusRuledOut)
		if !strings.Contains(h.Contradict[0].Text, "nothing holds") {
			t.Fatalf("%s contradiction = %+v", id, h.Contradict)
		}
	}
	// Saturated in only one sample: unproven, not a root.
	obs[0] = xidSample("E1", t0, 1e6, 2, 3, true)
	if d := DiagnoseWraparound(obs, "xid"); d.Root != nil &&
		d.Root.Node == AutovacuumSaturated {
		t.Fatalf("saturation in one sample became root: %+v", d.Root)
	}
}

func TestWraparound_AutovacuumDisabled(t *testing.T) {
	off := overdue
	off.enabled = false
	d := DiagnoseWraparound(wrapCase(off), "xid")
	root := requireStatus(t, d, AutovacuumDisabled, StatusRoot)
	if root.Subject != "table public.events" ||
		!strings.Contains(root.Support[0].Text, "autovacuum_enabled") {
		t.Fatalf("disabled root = %+v", root)
	}
	obs := wrapCase(overdue)
	obs[0] = xidSample("E1", t0, 1e6, 0, 3, false)
	obs[5] = xidSample("E6", t0.Add(5*time.Second), 1e6+25, 0, 3, false)
	d = DiagnoseWraparound(obs, "xid")
	root = requireStatus(t, d, AutovacuumDisabled, StatusRoot)
	if root.Subject != "autovacuum" {
		t.Fatalf("global off root = %+v", root)
	}
	sat := requireStatus(t, d, AutovacuumSaturated, StatusRuledOut)
	if !strings.Contains(sat.Contradict[0].Text, "autovacuum is off") {
		t.Fatalf("saturated contradiction = %+v", sat.Contradict)
	}
}

// Logged cancellations support a cause; their absence is never read as
// proof (the log source may be missing).
func TestWraparound_LoggedCancellations(t *testing.T) {
	obs := wrapCase(overdue)
	obs[3] = cancelObs("E4", 4)
	d := DiagnoseWraparound(obs, "xid")
	root := requireStatus(t, d, AutovacuumCancelled, StatusRoot)
	if !strings.Contains(root.Support[0].Text, "4") {
		t.Fatalf("cancelled root = %+v", root)
	}
	d = DiagnoseWraparound(wrapCase(overdue), "xid")
	h := requireStatus(t, d, AutovacuumCancelled, StatusAlternative)
	if len(h.Contradict) != 0 {
		t.Fatalf("zero logged cancellations refuted the hypothesis: %+v", h)
	}
}

func TestWraparound_XIDSurgeAmplifiesTheHolder(t *testing.T) {
	obs := wrapCase(overdue, wholder{kind: probes.HolderSession, pid: 4242,
		state: "idle in transaction", age: 149000})
	obs[4] = trendsObs("E5", trendRow(probes.RunwayXID, probes.SubjectCluster, 61,
		150000, 2107483647, 10, 1))
	obs[5] = xidSample("E6", t0.Add(5*time.Second), 1e6+20000, 1, 3, true)
	d := DiagnoseWraparound(obs, "xid")
	requireStatus(t, d, XminHeldBySession, StatusRoot)
	surge := requireStatus(t, d, XIDConsumptionSurge, StatusContributing)
	if !strings.Contains(surge.Support[0].Text, "4000") {
		t.Fatalf("surge = %+v, want the 4000 XIDs/s sample rate", surge)
	}
}

// Without a long-run trend a sample rate cannot be called a surge.
func TestWraparound_SurgeNeedsATrend(t *testing.T) {
	obs := wrapCase(overdue)
	obs[4] = trendsObs("E5")
	obs[5] = xidSample("E6", t0.Add(5*time.Second), 1e6+20000, 1, 3, true)
	d := DiagnoseWraparound(obs, "xid")
	requireStatus(t, d, XIDConsumptionSurge, StatusAlternative)
	if !hasMissing(d, probes.RunwayTrendsProbe, "xid_trend_unavailable") {
		t.Fatalf("missing = %+v, want the XID trend unavailable", d.Missing)
	}
}

// A measured runway to the wraparound warning limit inside the horizon
// is near even when no table is past half its maximum.
func TestWraparound_TrendProjectsTheWarningLimit(t *testing.T) {
	low := wtable{rel: "public.events", xid: 60000, max: 200000, enabled: true}
	obs := wrapCase(low, wholder{kind: probes.HolderSession, pid: 5, state: "active",
		age: 59000})
	obs[4] = trendsObs("E5", trendRow(probes.RunwayXID, probes.SubjectCluster, 61,
		2e9, 2107483647, 1000, 1))
	d := DiagnoseWraparound(obs, "xid")
	requireStatus(t, d, XminHeldBySession, StatusRoot)
	if !hasObserved(d, "E5", "107483.65", "2107483647") {
		t.Fatalf("observed = %+v, want the projected seconds to the limit", d.Observed)
	}
}

func TestWraparound_SubjectSelectsTheTable(t *testing.T) {
	obs := wrapCase(overdue, wholder{kind: probes.HolderSession, pid: 4, state: "active",
		age: 400})
	obs[1] = tablesObs("E2", overdue, wtable{rel: "public.b", xid: 1000, max: 1500,
		enabled: false})
	d := DiagnoseWraparound(obs, "table public.b")
	root := requireStatus(t, d, AutovacuumDisabled, StatusRoot)
	if root.Subject != "table public.b" {
		t.Fatalf("root = %+v, want the subject table", root)
	}
}

// Error propagation: unavailable evidence is missing, never healthy.
func TestWraparound_UnavailableEvidence(t *testing.T) {
	obs := wrapCase(overdue)
	obs[2] = unavailable("E3", probes.XminHorizon, probes.StatusNoPrivilege)
	d := DiagnoseWraparound(obs, "xid")
	for _, id := range []NodeID{XminHeldBySession, XminHeldByPreparedXact,
		XminHeldByReplication} {
		h := requireStatus(t, d, id, StatusAlternative)
		if len(h.Contradict) != 0 {
			t.Fatalf("%s refuted by an unreadable probe: %+v", id, h)
		}
	}
	if !hasMissingStatus(d, probes.XminHorizon, probes.StatusNoPrivilege) {
		t.Fatalf("missing = %+v", d.Missing)
	}
	obs[1] = unavailable("E2", probes.WraparoundTablesProbe, probes.StatusError)
	d = DiagnoseWraparound(obs, "xid")
	if d.Root != nil || !hasMissingStatus(d, probes.WraparoundTablesProbe,
		probes.StatusError) {
		t.Fatalf("no table evidence: root %+v missing %+v", d.Root, d.Missing)
	}
	d = DiagnoseWraparound(nil, "xid")
	if d.Root != nil || len(d.Missing) == 0 {
		t.Fatalf("no evidence: %+v", d)
	}
}

func hasObserved(d Diagnosis, ev string, parts ...string) bool {
	for _, f := range d.Observed {
		if f.EvidenceID != ev {
			continue
		}
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(f.Text, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func hasMissingStatus(d Diagnosis, id probes.ID, st probes.Status) bool {
	for _, m := range d.Missing {
		if m.ProbeID == id && m.Status == st {
			return true
		}
	}
	return false
}
