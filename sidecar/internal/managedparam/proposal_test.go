package managedparam

import (
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func intent(t *testing.T, provider, parameter, value string) Intent {
	t.Helper()
	i, err := NewIntent(provider, parameter, value, "requires a restart")
	if err != nil {
		t.Fatalf("NewIntent: %v", err)
	}
	return i
}

func rdsTarget() Target {
	return Target{Provider: "rds", Region: "us-east-1", InstanceID: "orders",
		ParameterGroup: "orders-pg16", ParameterGroupStatus: "in-sync", Known: true,
		Params: map[string]GroupParam{
			"shared_buffers": {Value: "{DBInstanceClassMemory/32768}", Source: "engine-default",
				ApplyType: "static", Modifiable: true},
			"work_mem":      {Value: "4096", Source: "user", ApplyType: "dynamic", Modifiable: true},
			"rds.force_ssl": {Value: "1", Source: "system", ApplyType: "dynamic"},
			"max_wal_size":  {Value: "2048", Source: "user", ApplyType: "dynamic", Modifiable: true},
		}}
}

func TestBuildRDSStaticParameter(t *testing.T) {
	running := &collector.PGSetting{Name: "shared_buffers", Setting: "131072", Unit: "8kB",
		Source: "configuration file"}
	p, err := Build(intent(t, "rds", "shared_buffers", "4GB"), rdsTarget(), running)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Mechanism != MechanismParameterGroup || p.Target != "orders-pg16" ||
		p.Value != "524288" || p.PGValue != "4GB" || p.Unit != "8kB" ||
		p.ApplyMethod != "pending-reboot" || !p.RebootRequired ||
		p.CurrentValue != "{DBInstanceClassMemory/32768}" || p.RunningValue != "131072" {
		t.Fatalf("proposal = %+v", p)
	}
	wantCLI := `aws rds modify-db-parameter-group --region us-east-1 ` +
		`--db-parameter-group-name orders-pg16 --parameters ` +
		`"ParameterName=shared_buffers,ParameterValue=524288,ApplyMethod=pending-reboot"` +
		"\n" + `aws rds reboot-db-instance --region us-east-1 --db-instance-identifier orders`
	if p.CLI != wantCLI {
		t.Fatalf("CLI =\n%s\nwant\n%s", p.CLI, wantCLI)
	}
	if !p.Rollback.Reset || !strings.HasPrefix(p.Rollback.CLI,
		"aws rds reset-db-parameter-group --region us-east-1 --db-parameter-group-name "+
			"orders-pg16 --parameters \"ParameterName=shared_buffers,ApplyMethod=pending-reboot\"") {
		t.Fatalf("engine-default rollback must reset: %+v", p.Rollback)
	}
	if !strings.Contains(p.ConsoleURL, "region=us-east-1") ||
		!strings.Contains(p.ConsoleURL, "orders-pg16") {
		t.Fatalf("console = %q", p.ConsoleURL)
	}
	assertApprovalOnly(t, p)
	if len(p.Fingerprint) != 16 || len(p.Blockers) != 0 {
		t.Fatalf("fingerprint=%q blockers=%v", p.Fingerprint, p.Blockers)
	}
}

// assertApprovalOnly is the invariant of this release: a managed change
// is never auto-applied, whatever the inputs.
func assertApprovalOnly(t *testing.T, p Proposal) {
	t.Helper()
	if !p.RequiresApproval || p.AutoApply || p.AutoApplyWithheld == "" {
		t.Fatalf("managed change must be approval-only: %+v", p)
	}
}

func TestBuildRDSDynamicParameterRollsBackToPriorValue(t *testing.T) {
	p, err := Build(intent(t, "rds", "work_mem", "64MB"), rdsTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Value != "65536" || p.ApplyMethod != "immediate" || p.RebootRequired ||
		strings.Contains(p.CLI, "reboot-db-instance") {
		t.Fatalf("dynamic proposal = %+v", p)
	}
	if p.Rollback.Reset || p.Rollback.Value != "4096" || !strings.Contains(p.Rollback.CLI,
		"ParameterName=work_mem,ParameterValue=4096,ApplyMethod=immediate") {
		t.Fatalf("user-set rollback must restore 4096: %+v", p.Rollback)
	}
	assertApprovalOnly(t, p)
}

func TestBuildRDSDefaultGroupIsBlocked(t *testing.T) {
	target := rdsTarget()
	target.ParameterGroup, target.ParameterGroupIsDefault = "default.postgres16", true
	p, err := Build(intent(t, "rds", "shared_buffers", "4GB"), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Blockers) == 0 || !strings.Contains(p.Blockers[0], "default.postgres16") ||
		!strings.Contains(p.Blockers[0], "create-db-parameter-group") {
		t.Fatalf("default group blockers = %v", p.Blockers)
	}
	assertApprovalOnly(t, p)
}

func TestBuildRDSUnresolvedTargetUsesPlaceholder(t *testing.T) {
	target := Target{Provider: "rds", Unresolved: "cloud telemetry unavailable: no credentials"}
	p, err := Build(intent(t, "rds", "max_connections", "500"), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.CLI, "<db-parameter-group>") || !strings.Contains(p.CLI,
		"<region>") || !p.RebootRequired || p.ApplyMethod != "pending-reboot" {
		t.Fatalf("unresolved proposal = %+v", p)
	}
	if len(p.Notes) == 0 || !strings.Contains(strings.Join(p.Notes, " "), "no credentials") {
		t.Fatalf("notes must say why the target is unknown: %v", p.Notes)
	}
	assertApprovalOnly(t, p)
}

func TestBuildRefusesUnmodifiableParameter(t *testing.T) {
	_, err := Build(intent(t, "rds", "rds.force_ssl", "0"), rdsTarget(), nil)
	if !errors.Is(err, ErrNotModifiable) {
		t.Fatalf("err = %v, want ErrNotModifiable", err)
	}
}

func TestBuildAurora(t *testing.T) {
	target := rdsTarget()
	target.Provider = "aurora"
	p, err := Build(intent(t, "aurora", "shared_buffers", "8GB"), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Value != "1048576" || !strings.Contains(strings.Join(p.Notes, " "), "cluster") {
		t.Fatalf("aurora proposal = %+v", p)
	}
}

func cloudSQLTarget() Target {
	return Target{Provider: "cloud-sql", Project: "proj-1", InstanceID: "main", Known: true,
		FlagsKnown: true, Flags: map[string]string{"work_mem": "65536",
			"log_min_duration_statement": "500"}}
}

func TestBuildCloudSQLFlagKeepsExistingFlags(t *testing.T) {
	p, err := Build(intent(t, "cloud-sql", "shared_buffers", "4GB"), cloudSQLTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wantCLI := "gcloud sql instances patch main --project=proj-1 --database-flags=" +
		"log_min_duration_statement=500,shared_buffers=524288,work_mem=65536"
	if p.Mechanism != MechanismDatabaseFlag || p.CLI != wantCLI || p.Target != "proj-1:main" ||
		!p.RebootRequired {
		t.Fatalf("cloud sql proposal CLI=%q %+v", p.CLI, p)
	}
	if !strings.Contains(strings.Join(p.Notes, " "), "restarts the instance") {
		t.Fatalf("notes must warn about the automatic restart: %v", p.Notes)
	}
	wantRollback := "gcloud sql instances patch main --project=proj-1 --database-flags=" +
		"log_min_duration_statement=500,work_mem=65536"
	if p.Rollback.CLI != wantRollback || !p.Rollback.Reset {
		t.Fatalf("rollback = %+v", p.Rollback)
	}
	assertApprovalOnly(t, p)
}

func TestBuildCloudSQLRollbackRestoresPriorFlagOrClears(t *testing.T) {
	p, err := Build(intent(t, "cloud-sql", "work_mem", "128MB"), cloudSQLTarget(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Value != "131072" || p.RebootRequired || p.Rollback.Value != "65536" ||
		!strings.Contains(p.Rollback.CLI, "work_mem=65536") {
		t.Fatalf("work_mem proposal = %+v", p)
	}
	only := Target{Provider: "cloud-sql", Project: "proj-1", InstanceID: "main",
		FlagsKnown: true, Flags: map[string]string{}, Known: true}
	p, err = Build(intent(t, "cloud-sql", "work_mem", "128MB"), only, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Rollback.CLI != "gcloud sql instances patch main --project=proj-1 "+
		"--clear-database-flags" {
		t.Fatalf("rollback of the only flag must clear flags: %q", p.Rollback.CLI)
	}
}

// --database-flags replaces the whole list: without the current flags the
// command would silently drop them, so the proposal is blocked.
func TestBuildCloudSQLUnknownFlagsIsBlocked(t *testing.T) {
	target := Target{Provider: "cloud-sql", Project: "proj-1", InstanceID: "main"}
	p, err := Build(intent(t, "cloud-sql", "work_mem", "64MB"), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Blockers) == 0 || !strings.Contains(p.Blockers[0], "replaces") {
		t.Fatalf("blockers = %v", p.Blockers)
	}
	assertApprovalOnly(t, p)
}

func TestBuildRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		intent Intent
		target Target
		want   error
	}{
		{"provider mismatch", Intent{Provider: "rds", Parameter: "work_mem", Value: "4MB"},
			cloudSQLTarget(), ErrUnsupportedProvider},
		{"unsupported provider", Intent{Provider: "neon", Parameter: "work_mem",
			Value: "4MB"}, Target{Provider: "neon"}, ErrUnsupportedProvider},
		{"bad unit", Intent{Provider: "rds", Parameter: "work_mem", Value: "4mb"},
			rdsTarget(), ErrInvalidValue},
		{"not a multiple", Intent{Provider: "rds", Parameter: "shared_buffers",
			Value: "4097B"}, rdsTarget(), ErrInvalidValue},
		{"injection", Intent{Provider: "rds", Parameter: "work_mem",
			Value: "4MB\"; aws iam"}, rdsTarget(), ErrInvalidValue},
		{"bad group name", Intent{Provider: "rds", Parameter: "work_mem", Value: "4MB"},
			Target{Provider: "rds", Region: "us-east-1", ParameterGroup: "g;rm -rf",
				Known: true}, ErrInvalidTarget},
		{"bad instance", Intent{Provider: "cloud-sql", Parameter: "work_mem", Value: "4MB"},
			Target{Provider: "cloud-sql", Project: "p1", InstanceID: "$(x)"},
			ErrInvalidTarget},
		{"zero value intent", Intent{}, rdsTarget(), ErrUnsupportedProvider},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(tc.intent, tc.target, nil); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// Property: approval-only holds for every supported provider, parameter
// kind and target state.
func TestBuildIsAlwaysApprovalOnly(t *testing.T) {
	targets := []Target{rdsTarget(), cloudSQLTarget(), {Provider: "rds"},
		{Provider: "cloud-sql", Project: "p-1", InstanceID: "i"}}
	targets[0].Provider = "rds"
	values := map[string]string{"shared_buffers": "2GB", "work_mem": "8MB",
		"max_connections": "300", "autovacuum_naptime": "30s", "jit": "off",
		"random_page_cost": "1.1"}
	for _, target := range targets {
		for param, value := range values {
			i, err := NewIntent(target.Provider, param, value, "r")
			if err != nil {
				t.Fatal(err)
			}
			p, err := Build(i, target, nil)
			if err != nil {
				t.Fatalf("%s=%s on %s: %v", param, value, target.Provider, err)
			}
			assertApprovalOnly(t, p)
		}
	}
}

func TestBuildFingerprintIdentity(t *testing.T) {
	a, _ := Build(intent(t, "rds", "work_mem", "64MB"), rdsTarget(), nil)
	b, _ := Build(intent(t, "rds", "work_mem", "65536kB"), rdsTarget(), nil)
	c, _ := Build(intent(t, "rds", "work_mem", "128MB"), rdsTarget(), nil)
	if a.Fingerprint != b.Fingerprint {
		t.Fatal("the same provider value must fingerprint the same")
	}
	if a.Fingerprint == c.Fingerprint {
		t.Fatal("a different value must fingerprint differently")
	}
}
