package managedparam

import (
	"strings"
	"testing"
)

// No concurrent access tests: Build is a pure function of its arguments.

func auroraTarget(cluster, role string) Target {
	t := rdsTarget()
	t.Provider, t.InstanceID, t.Cluster, t.InstanceRole = "aurora", "writer-1", cluster, role
	return t
}

// Happy path: an Aurora writer reached by its instance endpoint names the
// cluster and keeps the existing Aurora note unchanged.
func TestBuildAuroraInstanceNamesCluster(t *testing.T) {
	p, err := Build(intent(t, "aurora", "shared_buffers", "8GB"),
		auroraTarget("shop", InstanceRoleWriter), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	notes := strings.Join(p.Notes, " ")
	for _, want := range []string{"writer-1 is the writer of Aurora cluster shop",
		"modify-db-cluster-parameter-group"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("notes %q do not contain %q", notes, want)
		}
	}
	if strings.Contains(notes, "may differ") {
		t.Fatalf("a writer has no parameter-group warning: %q", notes)
	}
	if p.Target != "orders-pg16" || p.Instance != "writer-1" {
		t.Fatalf("proposal target %q instance %q", p.Target, p.Instance)
	}
}

// State transitions: a reader warns that its group may differ from the
// writer's; an unknown role says pg_sage could not tell.
func TestBuildAuroraReaderAndUnknownRoleNotes(t *testing.T) {
	cases := map[string][]string{
		InstanceRoleReader: {"writer-1 is a reader of Aurora cluster shop", "may differ",
			"writer's"},
		InstanceRoleUnknown: {"could not tell whether writer-1 is the writer",
			"cluster shop"},
	}
	for role, wants := range cases {
		p, err := Build(intent(t, "aurora", "shared_buffers", "8GB"),
			auroraTarget("shop", role), nil)
		if err != nil {
			t.Fatalf("%s: Build: %v", role, err)
		}
		notes := strings.Join(p.Notes, " ")
		for _, want := range wants {
			if !strings.Contains(notes, want) {
				t.Fatalf("%s: notes %q do not contain %q", role, notes, want)
			}
		}
	}
}

// Nil/empty: no cluster (an RDS target, or an Aurora target resolved
// before this change) adds no cluster note.
func TestBuildWithoutClusterAddsNoClusterNote(t *testing.T) {
	for _, target := range []Target{rdsTarget(), auroraTarget("", "")} {
		in := intent(t, target.Provider, "shared_buffers", "8GB")
		p, err := Build(in, target, nil)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		notes := strings.Join(p.Notes, " ")
		if strings.Contains(notes, "Aurora cluster") || strings.Contains(notes, "could not tell") {
			t.Fatalf("%s: unexpected cluster note %q", target.Provider, notes)
		}
	}
}

// Boundary: the cluster and role are notes only, never part of the
// proposal's identity, so a failover does not create a second proposal.
func TestBuildAuroraRoleDoesNotChangeFingerprint(t *testing.T) {
	in := intent(t, "aurora", "shared_buffers", "8GB")
	writer, err := Build(in, auroraTarget("shop", InstanceRoleWriter), nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := Build(in, auroraTarget("shop", InstanceRoleReader), nil)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := Build(in, auroraTarget("", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if writer.Fingerprint == "" || writer.Fingerprint != reader.Fingerprint ||
		writer.Fingerprint != bare.Fingerprint {
		t.Fatalf("fingerprints %q %q %q", writer.Fingerprint, reader.Fingerprint,
			bare.Fingerprint)
	}
}

// Invalid input: the target's provider must still match the intent's.
func TestBuildAuroraIntentRefusesRDSTarget(t *testing.T) {
	target := auroraTarget("shop", InstanceRoleWriter)
	target.Provider = "rds"
	if _, err := Build(intent(t, "aurora", "shared_buffers", "8GB"), target, nil); err == nil ||
		!strings.Contains(err.Error(), "intent for aurora") {
		t.Fatalf("err = %v, want a provider mismatch", err)
	}
}
