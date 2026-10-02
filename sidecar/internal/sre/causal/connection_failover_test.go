package causal

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// CHECK-07, connection pressure: idle backends that "grow" across a
// failover are clients reconnecting to the new primary, not a leak. The
// comparison is invalid and stated as missing evidence; nothing is
// scored from growth.

// connObsIdent is connObs with every row carrying the server identity.
func connObsIdent(id string, at time.Time, si probes.ServerIdentity, total int64,
	gs ...group) Observation {
	o := connObs(id, at, total, gs...)
	for _, r := range o.Result.Rows {
		r["server_started_at"] = si.StartedAt
		r["system_identifier"] = si.SystemID
		r["timeline_id"] = si.TimelineID
		r["in_recovery"] = si.Role == probes.ServerRoleStandby
		r["server_version_num"] = si.VersionNum
	}
	return o
}

func missingReason(d Diagnosis, probe probes.ID) string {
	for _, m := range d.Missing {
		if m.ProbeID == probe {
			return m.Reason
		}
	}
	return ""
}

func TestConnections_FailoverBetweenSamplesIsNotALeak(t *testing.T) {
	promoted := ident(serverStart.Add(30*time.Minute), "7001", 2, probes.ServerRolePrimary,
		170010)
	for name, second := range map[string]probes.ServerIdentity{
		"failover to a replica": promoted,
		"promotion in place": ident(serverStart, "7001", 2, probes.ServerRolePrimary,
			170010),
		"another server": ident(serverStart, "9999", 1, probes.ServerRolePrimary, 170010),
	} {
		t.Run(name, func(t *testing.T) {
			first := baseIdent
			if name == "promotion in place" {
				first = ident(serverStart, "7001", 1, probes.ServerRoleStandby, 170010)
			}
			d := DiagnoseConnections([]Observation{
				connObsIdent("E1", t0, first, 12, group{app: "etl", state: "idle", n: 2}),
				connObsIdent("E2", t0.Add(5*time.Second), second, 12,
					group{app: "etl", state: "idle", n: 9}),
			})
			if d.Conclusive || d.Root != nil {
				t.Fatalf("a failover between samples concluded %+v", d.Root)
			}
			leak, st := byNode(d, ConnectionLeak)
			if st == StatusRuledOut || leak.Confidence != 0 || len(leak.Support) != 0 {
				t.Fatalf("leak scored across a failover: %s %+v", st, leak)
			}
			want := ReasonFailover
			if name == "another server" {
				want = ReasonServerReplaced
			}
			if got := missingReason(d, probes.ConnectionSaturation); got != want {
				t.Fatalf("missing reason = %q, want %q (%+v)", got, want, d.Missing)
			}
		})
	}
}

// The same incarnation with the identity on every row still compares.
func TestConnections_SameIncarnationWithIdentityCompares(t *testing.T) {
	d := DiagnoseConnections([]Observation{
		connObsIdent("E1", t0, baseIdent, 12, group{app: "etl", state: "idle", n: 2}),
		connObsIdent("E2", t0.Add(5*time.Second), baseIdent, 12,
			group{app: "etl", state: "idle", n: 9}),
	})
	requireStatus(t, d, ConnectionLeak, StatusRoot)
	if got := missingReason(d, probes.ConnectionSaturation); got != "" {
		t.Fatalf("comparable samples reported missing %q", got)
	}
}

// A restart (start time only) keeps its M2 reason.
func TestConnections_RestartKeepsItsReason(t *testing.T) {
	restarted := ident(serverStart.Add(time.Hour), "7001", 1, probes.ServerRolePrimary,
		170010)
	d := DiagnoseConnections([]Observation{
		connObsIdent("E1", t0, baseIdent, 12, group{app: "etl", state: "idle", n: 2}),
		connObsIdent("E2", t0.Add(5*time.Second), restarted, 12,
			group{app: "etl", state: "idle", n: 9}),
	})
	if d.Conclusive {
		t.Fatalf("a restart between samples concluded %+v", d.Root)
	}
	if got := missingReason(d, probes.ConnectionSaturation); got != ReasonServerRestarted {
		t.Fatalf("missing reason = %q, want %q", got, ReasonServerRestarted)
	}
}

// A pool's fan-out is still judged from the last sample after a
// failover: what the new primary holds now is an observation, only the
// growth is not.
func TestConnections_FailoverStillJudgesTheLastSample(t *testing.T) {
	promoted := ident(serverStart.Add(time.Hour), "7001", 2, probes.ServerRolePrimary,
		170010)
	d := DiagnoseConnections([]Observation{
		connObsIdent("E1", t0, baseIdent, 36, group{app: "api", state: "idle", n: 30}),
		connObsIdent("E2", t0.Add(5*time.Second), promoted, 36,
			group{app: "api", state: "idle", n: 30}),
	})
	fan, st := byNode(d, PoolFanOut)
	if st == StatusRuledOut || fan.Confidence < 0.5 {
		t.Fatalf("fan-out from the last sample = %s %+v", st, fan)
	}
	for _, f := range fan.Support {
		if f.EvidenceID != "E2" {
			t.Fatalf("fan-out cites the pre-failover sample: %+v", fan.Support)
		}
	}
}
