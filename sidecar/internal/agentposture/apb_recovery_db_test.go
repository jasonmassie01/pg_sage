package agentposture

import "testing"

// AP-11 on the self-managed test servers: archive_mode is off (the test
// containers do not archive WAL), so there is no PITR; with an agent role
// and no pgaudit, the pgaudit arm reports too. On PG14 the archive_library
// arm is skipped with its reason.
func TestAP11_SelfManagedServer(t *testing.T) {
	f := newFixture(t)
	var mode string
	if err := f.pool.QueryRow(f.ctx, "SHOW archive_mode").Scan(&mode); err != nil {
		t.Fatalf("archive_mode: %v", err)
	}
	agent := registeredRoleName(t)
	createRole(t, f.ctx, f.pool, agent, "NOLOGIN")
	env := f.env(func(e *Env) { f.agent(e, agent, SourceRegistered) })
	o := f.run("AP-11", env)
	if mode == "off" {
		got := requireFinding(t, o, "archive_mode", Warning)
		requireContains(t, "AP-11 fix", got.FixScript, "archive_mode")
	} else {
		requireNoFinding(t, o, "archive_mode")
	}
	got := requireFinding(t, o, "pgaudit", Warning)
	requireContains(t, "AP-11 pgaudit detail", got.Detail, "agent")
	if env.VersionNum < 150000 {
		requireSkipped(t, o, "archive_library")
	} else if len(o.Skipped) != 0 {
		t.Fatalf("AP-11 skipped arms on PG15+: %+v", o.Skipped)
	}
}

// AP-11 without agent roles leaves pgaudit alone.
func TestAP11_NoAgentsNoPgauditFinding(t *testing.T) {
	f := newFixture(t)
	requireNoFinding(t, f.run("AP-11", f.env(func(e *Env) { e.Agents = nil })), "pgaudit")
}

// AP-11 on a provider reads the recorded backup posture, not archive_mode:
// RDS with retention 0 and no deletion protection reports both; unknown
// telemetry reports neither.
func TestAP11_ProviderUsesCloudTelemetry(t *testing.T) {
	f := newFixture(t)
	env := f.env(func(e *Env) {
		e.Config.Platform = Platform{Provider: "rds", Backup: &BackupPosture{
			RetentionDays: intp(0), DeletionProtection: boolp(false)}}
	})
	o := f.run("AP-11", env)
	requireNoFinding(t, o, "archive_mode")
	requireFinding(t, o, "backups", Warning)
	requireFinding(t, o, "deletion_protection", Warning)

	env.Config.Platform.Backup = nil
	o = f.run("AP-11", env)
	requireNoFinding(t, o, "backups")
	requireNoFinding(t, o, "deletion_protection")
	requireNoFinding(t, o, "archive_mode")
}
