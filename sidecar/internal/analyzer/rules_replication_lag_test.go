package analyzer

import (
	"sort"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// No concurrent-access tests: ruleReplicationLag is a pure function of
// the snapshot and keeps no state.

func lagReplica(app, addr, lag string) collector.ReplicaInfo {
	r := collector.ReplicaInfo{ApplicationName: app, State: "streaming"}
	if addr != "" {
		r.ClientAddr = strPtr(addr)
	}
	if lag != "" {
		r.ReplayLag = strPtr(lag)
	}
	return r
}

func lagSnap(replicas ...collector.ReplicaInfo) *collector.Snapshot {
	return &collector.Snapshot{
		Replication: &collector.ReplicationStats{Replicas: replicas},
	}
}

func lagIdents(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.ObjectIdentifier)
	}
	sort.Strings(out)
	return out
}

func TestReplicationLag_IdentityStableAcrossRowOrder(t *testing.T) {
	a := lagReplica("standby_a", "10.0.0.1", "00:01:00")
	b := lagReplica("standby_b", "10.0.0.2", "00:02:00")
	first := ruleReplicationLag(lagSnap(a, b), nil, nil, nil)
	second := ruleReplicationLag(lagSnap(b, a), nil, nil, nil)
	want := []string{"replica:standby_a@10.0.0.1", "replica:standby_b@10.0.0.2"}
	for i, got := range [][]string{lagIdents(first), lagIdents(second)} {
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("cycle %d identities = %v, want %v", i+1, got, want)
		}
	}
	for _, f := range second {
		if f.ObjectIdentifier == want[1] && f.Detail["replay_lag"] != "00:02:00" {
			t.Fatalf("%s carries the other replica's lag %v", want[1], f.Detail["replay_lag"])
		}
	}
}

func TestReplicationLag_DuplicateKeyMergesAtWorstLag(t *testing.T) {
	snap := lagSnap(
		lagReplica("walreceiver", "10.0.0.9", "00:00:40"),
		lagReplica("walreceiver", "10.0.0.9", "00:06:00"),
		lagReplica("walreceiver", "10.0.0.9", "00:00:01"),
	)
	got := ruleReplicationLag(snap, nil, nil, nil)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want one merged finding", len(got))
	}
	f := got[0]
	if f.ObjectIdentifier != "replica:walreceiver@10.0.0.9" || f.Severity != "critical" {
		t.Fatalf("finding = %s %s, want critical replica:walreceiver@10.0.0.9",
			f.Severity, f.ObjectIdentifier)
	}
	if f.Detail["replay_lag"] != "00:06:00" || f.Detail["merged_replicas"] != 3 {
		t.Fatalf("detail = %v, want worst lag 00:06:00 over 3 replicas", f.Detail)
	}
	if f.Detail["application_name"] != "walreceiver" {
		t.Fatalf("application_name = %v", f.Detail["application_name"])
	}
}

func TestReplicationLag_MissingNameAndAddress(t *testing.T) {
	got := ruleReplicationLag(lagSnap(lagReplica("", "", "00:01:00")), nil, nil, nil)
	if len(got) != 1 || got[0].ObjectIdentifier != "replica:<unnamed>@<unknown>" {
		t.Fatalf("got %v, want replica:<unnamed>@<unknown>", lagIdents(got))
	}
}

func TestReplicationLag_NoReplicasNoFinding(t *testing.T) {
	if got := ruleReplicationLag(lagSnap(), nil, nil, nil); len(got) != 0 {
		t.Fatalf("no replicas gave %d findings", len(got))
	}
	// Below 30 s on every replica of a key: nothing, even when merged.
	snap := lagSnap(lagReplica("s", "10.0.0.1", "00:00:29"),
		lagReplica("s", "10.0.0.1", ""))
	if got := ruleReplicationLag(snap, nil, nil, nil); len(got) != 0 {
		t.Fatalf("sub-threshold lag gave %d findings", len(got))
	}
}
