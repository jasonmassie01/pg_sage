package managedparam

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// No concurrent access tests: fingerprint is a pure function of its input.

// v2Fingerprint is the documented identity, computed independently of the
// package: "v2:" plus the first 16 hex digits of sha256(provider|parameter|value).
func v2Fingerprint(provider, parameter, value string) string {
	sum := sha256.Sum256([]byte(provider + "|" + parameter + "|" + value))
	return "v2:" + hex.EncodeToString(sum[:])[:16]
}

func TestFingerprintIsV2ProviderParameterValue(t *testing.T) {
	p, err := Build(intent(t, "rds", "work_mem", "64MB"), rdsTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := v2Fingerprint("rds", "work_mem", "65536"); p.Fingerprint != want {
		t.Fatalf("fingerprint = %q, want %q", p.Fingerprint, want)
	}
	if len(p.Fingerprint) != 19 || !strings.HasPrefix(p.Fingerprint, "v2:") {
		t.Fatalf("fingerprint %q must be v2: plus 16 hex digits", p.Fingerprint)
	}
}

// The target is a refreshable attribute: a resolved, a placeholder and a
// different resolved target give the same identity.
func TestFingerprintIgnoresTarget(t *testing.T) {
	in := intent(t, "rds", "work_mem", "64MB")
	resolved, err := Build(in, rdsTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	placeholder, err := Build(in, Target{Provider: "rds", Unresolved: "throttled"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := rdsTarget()
	other.ParameterGroup, other.Region = "orders-pg17", "eu-west-1"
	moved, err := Build(in, other, nil)
	if err != nil {
		t.Fatal(err)
	}
	if placeholder.Target == resolved.Target || moved.Target == resolved.Target {
		t.Fatalf("targets must differ for this test: %q %q %q", resolved.Target,
			placeholder.Target, moved.Target)
	}
	if placeholder.Fingerprint != resolved.Fingerprint ||
		moved.Fingerprint != resolved.Fingerprint {
		t.Fatalf("fingerprints %q %q %q must not depend on the target",
			resolved.Fingerprint, placeholder.Fingerprint, moved.Fingerprint)
	}
}

func TestFingerprintDistinguishesProviderParameterValue(t *testing.T) {
	base, _ := Build(intent(t, "rds", "work_mem", "64MB"), rdsTarget(), nil)
	aurora := rdsTarget()
	aurora.Provider = "aurora"
	cases := map[string]Proposal{}
	cases["provider"], _ = Build(intent(t, "aurora", "work_mem", "64MB"), aurora, nil)
	cases["parameter"], _ = Build(intent(t, "rds", "maintenance_work_mem", "64MB"),
		rdsTarget(), nil)
	cases["value"], _ = Build(intent(t, "rds", "work_mem", "65MB"), rdsTarget(), nil)
	for name, p := range cases {
		if p.Fingerprint == "" || p.Fingerprint == base.Fingerprint {
			t.Errorf("a different %s must fingerprint differently: %q vs %q", name,
				p.Fingerprint, base.Fingerprint)
		}
	}
}

func TestIsPlaceholderTarget(t *testing.T) {
	in := intent(t, "rds", "work_mem", "64MB")
	resolved, _ := Build(in, rdsTarget(), nil)
	unresolved, _ := Build(in, Target{Provider: "rds"}, nil)
	sql, _ := Build(intent(t, "cloud-sql", "work_mem", "64MB"), Target{Provider: "cloud-sql"},
		nil)
	if resolved.placeholderTarget() {
		t.Fatalf("resolved target %q read as a placeholder", resolved.Target)
	}
	if !unresolved.placeholderTarget() || !sql.placeholderTarget() {
		t.Fatalf("placeholders %q and %q not recognised", unresolved.Target, sql.Target)
	}
	if (Proposal{}).placeholderTarget() != true {
		t.Fatal("an empty target is not a resolved one")
	}
}
