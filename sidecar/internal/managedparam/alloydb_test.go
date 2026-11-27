package managedparam

import (
	"errors"
	"strings"
	"testing"
)

// advisors-03: AlloyDB database flags become typed proposals. Until
// clouds-09 reads an instance's flags, every AlloyDB target has
// FlagsKnown=false, so the proposal is clouds-01's fill-in template.
//
// No concurrent access tests: buildAlloyDB is a pure function.

func alloyTarget() Target {
	return Target{Provider: "alloydb", Project: "my-proj-1", Region: "us-central1",
		Cluster: "orders", InstanceID: "orders-primary", Known: true, FlagsKnown: true,
		Flags: map[string]string{"work_mem": "4096"}}
}

const alloyUpdate = "gcloud alloydb instances update orders-primary --cluster=orders " +
	"--region=us-central1 --project=my-proj-1"

func TestBuildAlloyDBUnknownFlagsGivesATemplate(t *testing.T) {
	p, err := Build(intent(t, "alloydb", "max_connections", "500"),
		Target{Provider: "alloydb", Unresolved: "cloud telemetry is not configured"}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Provider != "alloydb" || p.Mechanism != MechanismDatabaseFlag ||
		p.Value != "500" || !p.RebootRequired || !p.placeholderTarget() {
		t.Fatalf("proposal = %+v", p)
	}
	for _, cli := range []string{p.CLI, p.Rollback.CLI} {
		if !strings.HasPrefix(cli, "#") || strings.Contains(cli, "--database-flags=") {
			t.Fatalf("unknown flags must give a comment template, got %q", cli)
		}
	}
	for _, want := range []string{"gcloud alloydb instances describe <instance>",
		"--cluster=<cluster>", "--region=<region>", "max_connections=500"} {
		if !strings.Contains(p.CLI, want) {
			t.Fatalf("template lacks %q: %q", want, p.CLI)
		}
	}
	if len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "flags are unknown") ||
		!strings.Contains(p.Blockers[0], "cloud telemetry is not configured") {
		t.Fatalf("blockers = %v", p.Blockers)
	}
	assertApprovalOnly(t, p)
}

// With the flags known (clouds-09) the command carries every current flag
// with the change, quoted, and the rollback restores the list.
func TestBuildAlloyDBKnownFlagsCarriesTheWholeList(t *testing.T) {
	p, err := Build(intent(t, "alloydb", "max_connections", "500"), alloyTarget(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.CLI != alloyUpdate+" --database-flags='max_connections=500,work_mem=4096'" {
		t.Fatalf("CLI = %q", p.CLI)
	}
	if !p.Rollback.Reset || p.Rollback.Value != "" ||
		p.Rollback.CLI != alloyUpdate+" --database-flags='work_mem=4096'" {
		t.Fatalf("rollback = %+v", p.Rollback)
	}
	if p.Instance != "orders-primary" || p.Cluster != "orders" || p.Region != "us-central1" ||
		p.Project != "my-proj-1" || len(p.Blockers) != 0 ||
		!strings.Contains(p.ConsoleURL, "alloydb") {
		t.Fatalf("proposal = %+v", p)
	}
	// A changed flag rolls back to its prior value.
	p, err = Build(intent(t, "alloydb", "work_mem", "8MB"), alloyTarget(), nil)
	if err != nil || p.CurrentValue != "4096" || p.Rollback.Value != "4096" ||
		p.Rollback.Reset || p.CLI != alloyUpdate+" --database-flags='work_mem=8192'" {
		t.Fatalf("work_mem: %v %+v", err, p)
	}
}

// An instance without flags has no list to restore: no clearing command
// is guessed, the rollback names the console.
func TestBuildAlloyDBEmptyFlagListRollback(t *testing.T) {
	target := alloyTarget()
	target.Flags = map[string]string{}
	p, err := Build(intent(t, "alloydb", "max_connections", "500"), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.CLI != alloyUpdate+" --database-flags='max_connections=500'" ||
		!strings.HasPrefix(p.Rollback.CLI, "#") {
		t.Fatalf("CLI=%q rollback=%q", p.CLI, p.Rollback.CLI)
	}
}

func TestBuildAlloyDBUnsafeFlagWithholdsCommands(t *testing.T) {
	target := alloyTarget()
	target.Flags["search_path"] = "a b"
	p, err := Build(intent(t, "alloydb", "max_connections", "500"), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !strings.HasPrefix(p.CLI, "#") || !strings.HasPrefix(p.Rollback.CLI, "#") ||
		len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "search_path") {
		t.Fatalf("CLI=%q rollback=%q blockers=%v", p.CLI, p.Rollback.CLI, p.Blockers)
	}
}

func TestBuildAlloyDBRefusesUnsafeTargets(t *testing.T) {
	cases := map[string]func(*Target){
		"region":   func(t *Target) { t.Region = "us-central1;id" },
		"aws form": func(t *Target) { t.Region = "us east" },
		"cluster":  func(t *Target) { t.Cluster = "Orders_1" },
		"instance": func(t *Target) { t.InstanceID = "orders primary" },
		"project":  func(t *Target) { t.Project = "P" },
	}
	for name, mutate := range cases {
		target := alloyTarget()
		mutate(&target)
		if _, err := Build(intent(t, "alloydb", "work_mem", "8MB"), target,
			nil); !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("%s: err = %v, want ErrInvalidTarget", name, err)
		}
	}
}

func TestBuildAlloyDBBooleanIsOnOff(t *testing.T) {
	p, err := Build(intent(t, "alloydb", "jit", "true"), alloyTarget(), nil)
	if err != nil || p.Value != "on" {
		t.Fatalf("jit = %q, %v; want on", p.Value, err)
	}
}
