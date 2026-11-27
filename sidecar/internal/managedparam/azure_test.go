package managedparam

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// advisors-03: Azure flexible-server parameters become typed proposals with
// the az command; pg_sage never writes them (only the WAL-slot custodian's
// max_slot_wal_keep_size goes through ARM).
//
// No concurrent access tests: Build and buildAzure are pure functions of
// their arguments.

const azureSub = "00000000-0000-0000-0000-0000000000a1"

func azureTarget() Target {
	return Target{Provider: "azure", Subscription: azureSub, ResourceGroup: "rg-orders",
		InstanceID: "orders-flex", Known: true,
		Params: map[string]GroupParam{
			"shared_buffers": {Value: "131072", Default: "16384", Source: "user",
				Unit: "8kB", ApplyType: "static", Modifiable: true},
			"work_mem": {Value: "4096", Default: "4096", Source: "system", Unit: "KB",
				ApplyType: "dynamic", Modifiable: true},
			"max_connections": {Value: "100", Default: "100", Source: "system",
				ApplyType: "static", Modifiable: false},
		}}
}

const azSet = "az postgres flexible-server parameter set --subscription " + azureSub +
	" --resource-group 'rg-orders' --server-name orders-flex"

func TestBuildAzureStaticParameterConvertsAndRestarts(t *testing.T) {
	running := &collector.PGSetting{Name: "shared_buffers", Setting: "131072", Unit: "8kB"}
	p, err := Build(intent(t, "azure", "shared_buffers", "4GB"), azureTarget(), running)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Provider != "azure" || p.Mechanism != MechanismServerParameter ||
		p.Value != "524288" || p.PGValue != "4GB" || p.Unit != "8kB" ||
		!p.RebootRequired || p.ApplyMethod != ApplyPendingReboot ||
		p.CurrentValue != "131072" || p.RunningValue != "131072" ||
		p.Instance != "orders-flex" || p.ResourceGroup != "rg-orders" ||
		p.Target != "rg-orders/orders-flex" {
		t.Fatalf("proposal = %+v", p)
	}
	wantCLI := azSet + " --name shared_buffers --value 524288\n" +
		"az postgres flexible-server restart --subscription " + azureSub +
		" --resource-group 'rg-orders' --name orders-flex"
	if p.CLI != wantCLI {
		t.Fatalf("CLI =\n%s\nwant\n%s", p.CLI, wantCLI)
	}
	// The prior user value is restored, not the default.
	want := Rollback{Value: "131072", Reset: false, ApplyMethod: ApplyPendingReboot,
		CLI: azSet + " --name shared_buffers --value '131072'"}
	if p.Rollback != want {
		t.Fatalf("rollback = %+v, want %+v", p.Rollback, want)
	}
	if len(p.Blockers) != 0 || !strings.Contains(p.ConsoleURL, "orders-flex") ||
		!strings.Contains(p.ConsoleURL, "serverParameters") {
		t.Fatalf("blockers=%v console=%q", p.Blockers, p.ConsoleURL)
	}
	if !containsText(p.Notes, "restart") {
		t.Fatalf("a static parameter must note the restart: %v", p.Notes)
	}
	assertApprovalOnly(t, p)
}

// A dynamic parameter whose source is the system default rolls back with
// --source system-default and the captured default.
func TestBuildAzureDynamicParameterRollsBackToDefault(t *testing.T) {
	p, err := Build(intent(t, "azure", "work_mem", "64MB"), azureTarget(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Value != "65536" || p.Unit != "KB" || p.RebootRequired ||
		p.ApplyMethod != ApplyImmediate || strings.Contains(p.CLI, "restart") {
		t.Fatalf("proposal = %+v", p)
	}
	if p.CLI != azSet+" --name work_mem --value 65536" {
		t.Fatalf("CLI = %q", p.CLI)
	}
	want := Rollback{Value: "4096", Reset: true, ApplyMethod: ApplyImmediate,
		CLI: azSet + " --name work_mem --value '4096' --source system-default"}
	if p.Rollback != want {
		t.Fatalf("rollback = %+v, want %+v", p.Rollback, want)
	}
	assertApprovalOnly(t, p)
}

// The unit Azure reports wins over PostgreSQL's base unit.
func TestBuildAzureUsesTheReportedUnit(t *testing.T) {
	target := azureTarget()
	target.Params["max_wal_size"] = GroupParam{Value: "1048576", Default: "1048576",
		Source: "system", Unit: "KB", ApplyType: "dynamic", Modifiable: true}
	p, err := Build(intent(t, "azure", "max_wal_size", "4GB"), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Value != "4194304" || p.Unit != "KB" {
		t.Fatalf("value = %q %q, want 4194304 KB", p.Value, p.Unit)
	}
	// The running value is compared in PostgreSQL's unit, not Azure's.
	if got := runningForm(p); got != "4096" {
		t.Fatalf("runningForm = %q, want 4096 (MB, as pg_settings shows it)", got)
	}
	// Without a description the base unit (MB) is used.
	delete(target.Params, "max_wal_size")
	target.Known = false
	p, err = Build(intent(t, "azure", "max_wal_size", "4GB"), target, nil)
	if err != nil || p.Value != "4096" || p.Unit != "MB" {
		t.Fatalf("undescribed value = %q %q, %v; want 4096 MB", p.Value, p.Unit, err)
	}
}

// Other mechanisms already carry pg_settings' form.
func TestRunningFormKeepsNonAzureValues(t *testing.T) {
	p := Proposal{Provider: "rds", Mechanism: MechanismParameterGroup,
		Parameter: "max_wal_size", Value: "4096", PGValue: "4GB"}
	if got := runningForm(p); got != "4096" {
		t.Fatalf("runningForm = %q, want the proposal value", got)
	}
	p.Value, p.PGValue = "8", "not a value!" // never re-derived off Azure
	if got := runningForm(p); got != "8" {
		t.Fatalf("runningForm = %q, want 8", got)
	}
}

func TestBuildAzureBooleanIsOnOff(t *testing.T) {
	target := azureTarget()
	target.Params["jit"] = GroupParam{Value: "on", Default: "on", Source: "system",
		ApplyType: "dynamic", Modifiable: true}
	p, err := Build(intent(t, "azure", "jit", "off"), target, nil)
	if err != nil || p.Value != "off" {
		t.Fatalf("jit = %q, %v; want off", p.Value, err)
	}
}

func TestBuildAzureReadOnlyParameterIsNotModifiable(t *testing.T) {
	_, err := Build(intent(t, "azure", "max_connections", "500"), azureTarget(), nil)
	if !errors.Is(err, ErrNotModifiable) || !strings.Contains(err.Error(), "max_connections") {
		t.Fatalf("err = %v, want ErrNotModifiable naming the parameter", err)
	}
}

func TestBuildAzureRefusesUnsafeTargets(t *testing.T) {
	cases := map[string]func(*Target){
		"server with ;":         func(t *Target) { t.InstanceID = "orders;reboot" },
		"server with space":     func(t *Target) { t.InstanceID = "orders flex" },
		"upper-case server":     func(t *Target) { t.InstanceID = "Orders" },
		"resource group quote":  func(t *Target) { t.ResourceGroup = "rg'x" },
		"resource group dollar": func(t *Target) { t.ResourceGroup = "rg$(id)" },
		"resource group dot":    func(t *Target) { t.ResourceGroup = "rg." },
		"subscription":          func(t *Target) { t.Subscription = "sub;x" },
	}
	for name, mutate := range cases {
		target := azureTarget()
		mutate(&target)
		_, err := Build(intent(t, "azure", "work_mem", "64MB"), target, nil)
		if !errors.Is(err, ErrInvalidTarget) {
			t.Errorf("%s: err = %v, want ErrInvalidTarget", name, err)
		}
	}
	// A resource group may hold parentheses: it is quoted, not refused.
	target := azureTarget()
	target.ResourceGroup = "rg(prod)"
	p, err := Build(intent(t, "azure", "work_mem", "64MB"), target, nil)
	if err != nil || !strings.Contains(p.CLI, "--resource-group 'rg(prod)'") {
		t.Fatalf("rg(prod): %v %q", err, p.CLI)
	}
}

// Without the azure: section there is no server: the proposal is a
// fill-in template with a blocker, never a runnable command.
func TestBuildAzureUnresolvedTargetIsATemplate(t *testing.T) {
	target := Target{Provider: "azure", Unresolved: "cloud telemetry is not configured"}
	p, err := Build(intent(t, "azure", "shared_buffers", "4GB"), target, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Value != "524288" || p.Unit != "8kB" || !p.RebootRequired ||
		!p.placeholderTarget() {
		t.Fatalf("proposal = %+v", p)
	}
	for _, cli := range []string{p.CLI, p.Rollback.CLI} {
		for _, line := range strings.Split(cli, "\n") {
			if !strings.HasPrefix(line, "#") {
				t.Fatalf("runnable line %q in an unresolved proposal:\n%s", line, cli)
			}
		}
	}
	for _, want := range []string{"<resource-group>", "<server>", "--value 524288"} {
		if !strings.Contains(p.CLI, want) {
			t.Fatalf("template lacks %q:\n%s", want, p.CLI)
		}
	}
	if len(p.Blockers) != 1 || !strings.Contains(p.Blockers[0], "azure") ||
		!strings.Contains(p.Blockers[0], "cloud telemetry is not configured") {
		t.Fatalf("blockers = %v", p.Blockers)
	}
	assertApprovalOnly(t, p)
}

// A resolved server that did not describe the parameter (ARM 404, not
// asked) keeps the command but blocks it until the operator checks.
func TestBuildAzureUndescribedParameterBlocks(t *testing.T) {
	p, err := Build(intent(t, "azure", "max_wal_size", "4GB"), azureTarget(), nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.CLI != azSet+" --name max_wal_size --value 4096" || len(p.Blockers) != 1 ||
		!strings.Contains(p.Blockers[0], "max_wal_size") ||
		!strings.Contains(p.Blockers[0], "parameter show") {
		t.Fatalf("CLI=%q blockers=%v", p.CLI, p.Blockers)
	}
	if strings.HasPrefix(p.Rollback.CLI, "az ") {
		t.Fatalf("no prior value is known, so no rollback command: %q", p.Rollback.CLI)
	}
}

func TestBuildAzureUnsafeOrMissingPriorValueWithholdsRollback(t *testing.T) {
	cases := map[string]GroupParam{
		"unsafe user value": {Value: "a b", Source: "user", Unit: "KB",
			ApplyType: "dynamic", Modifiable: true},
		"empty default": {Value: "4096", Source: "system", Unit: "KB",
			ApplyType: "dynamic", Modifiable: true},
	}
	for name, gp := range cases {
		target := azureTarget()
		target.Params["work_mem"] = gp
		p, err := Build(intent(t, "azure", "work_mem", "64MB"), target, nil)
		if err != nil {
			t.Fatalf("%s: Build: %v", name, err)
		}
		if !strings.HasPrefix(p.Rollback.CLI, "# no rollback command") ||
			len(p.Blockers) != 1 || p.CLI != azSet+" --name work_mem --value 65536" {
			t.Errorf("%s: rollback=%+v blockers=%v cli=%q", name, p.Rollback, p.Blockers,
				p.CLI)
		}
		if strings.Contains(p.Rollback.CLI, "a b") {
			t.Errorf("%s: unsafe value leaked: %q", name, p.Rollback.CLI)
		}
	}
}

// An intent for another provider never builds against an Azure target.
func TestBuildAzureRejectsProviderMismatch(t *testing.T) {
	_, err := Build(intent(t, "rds", "work_mem", "64MB"), azureTarget(), nil)
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Fatalf("err = %v, want ErrUnsupportedProvider", err)
	}
}

func containsText(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l), want) {
			return true
		}
	}
	return false
}
