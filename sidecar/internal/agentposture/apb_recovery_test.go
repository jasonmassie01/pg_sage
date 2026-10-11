package agentposture

import (
	"testing"
)

func boolp(b bool) *bool { return &b }
func intp(i int) *int    { return &i }

func TestSelfManagedRecovery(t *testing.T) {
	cases := []struct {
		name string
		s    walArchiving
		want bool
		says string
	}{
		{"archiving off", walArchiving{mode: "off"}, true, "archive_mode is off"},
		{"on with a command", walArchiving{mode: "on", command: "cp %p /a/%f",
			commandKnown: true}, false, ""},
		{"on with a library", walArchiving{mode: "on", library: "basic_archive",
			libraryKnown: true, commandKnown: true}, false, ""},
		{"always with a command", walArchiving{mode: "always", command: "x",
			commandKnown: true}, false, ""},
		{"on but nothing archives", walArchiving{mode: "on", commandKnown: true,
			libraryKnown: true}, true, "no archive_command"},
		{"on, command unreadable", walArchiving{mode: "on"}, false, ""},
		{"mode unknown", walArchiving{}, false, ""},
	}
	for _, c := range cases {
		f, ok := selfManagedRecovery(c.s)
		if ok != c.want {
			t.Errorf("%s: finding %v, want %v (%+v)", c.name, ok, c.want, f)
			continue
		}
		if ok {
			if f.Severity != Warning || f.Object != "archive_mode" || f.FixScript == "" ||
				f.Caveat == "" || len(f.Evidence) == 0 {
				t.Errorf("%s: finding %+v", c.name, f)
			}
			requireContains(t, c.name+" detail", f.Detail, c.says)
		}
	}
}

func TestProviderRecovery(t *testing.T) {
	cases := []struct {
		name     string
		p        Platform
		pitr     bool
		deletion bool
	}{
		{"rds retention 0, unprotected", Platform{Provider: "rds", Backup: &BackupPosture{
			RetentionDays: intp(0), DeletionProtection: boolp(false)}}, true, true},
		{"rds retention 7, protected", Platform{Provider: "rds", Backup: &BackupPosture{
			RetentionDays: intp(7), DeletionProtection: boolp(true)}}, false, false},
		{"rds retention 1 is the boundary", Platform{Provider: "rds", Backup: &BackupPosture{
			RetentionDays: intp(1)}}, false, false},
		{"cloud sql backups off", Platform{Provider: "cloud-sql", Backup: &BackupPosture{
			BackupsEnabled: boolp(false), PITREnabled: boolp(true),
			DeletionProtection: boolp(true)}}, true, false},
		{"cloud sql pitr off", Platform{Provider: "cloud-sql", Backup: &BackupPosture{
			BackupsEnabled: boolp(true), PITREnabled: boolp(false),
			RetentionDays: intp(7)}}, true, false},
		{"cloud sql all on", Platform{Provider: "cloud-sql", Backup: &BackupPosture{
			BackupsEnabled: boolp(true), PITREnabled: boolp(true), RetentionDays: intp(7),
			DeletionProtection: boolp(true)}}, false, false},
		{"telemetry unknown", Platform{Provider: "rds"}, false, false},
		{"unknown fields", Platform{Provider: "aurora", Backup: &BackupPosture{}}, false,
			false},
	}
	for _, c := range cases {
		fs := providerRecovery(c.p)
		requireArm(t, fs, "backups", c.pitr, c.name)
		requireArm(t, fs, "deletion_protection", c.deletion, c.name)
		for _, f := range fs {
			if f.Severity != Warning || f.ObjectType != "instance" || f.FixScript == "" ||
				len(f.Evidence) == 0 || f.Caveat == "" {
				t.Errorf("%s: finding %+v", c.name, f)
			}
			requireContains(t, c.name+" evidence", f.Evidence[0].Source, "cloud telemetry")
		}
	}
}

func TestPlatformSelfManaged(t *testing.T) {
	for p, want := range map[string]bool{"": true, "self-managed": true, "unknown": true,
		"rds": false, "aurora": false, "cloud-sql": false, "alloydb": false, "azure": false,
		"neon": false, "supabase": false} {
		if got := (Platform{Provider: p}).selfManaged(); got != want {
			t.Errorf("Platform{%q}.selfManaged() = %v, want %v", p, got, want)
		}
	}
}

func TestPgauditFinding(t *testing.T) {
	agents := Env{Agents: []Role{{OID: 10, Name: "a", Source: SourceRegistered}}}
	cases := []struct {
		name string
		env  Env
		a    auditState
		want bool
	}{
		{"agents, absent", agents, auditState{preloadKnown: true, preload: "pg_stat_statements"},
			true},
		{"agents, extension", agents, auditState{extension: true}, false},
		{"agents, preloaded", agents, auditState{preloadKnown: true,
			preload: "pg_stat_statements, pgaudit"}, false},
		{"agents, preload unreadable", agents, auditState{}, false},
		{"no agents", Env{}, auditState{preloadKnown: true}, false},
	}
	for _, c := range cases {
		f, ok := pgauditFinding(c.env, c.a)
		if ok != c.want {
			t.Errorf("%s: finding %v, want %v", c.name, ok, c.want)
			continue
		}
		if ok && (f.Object != "pgaudit" || f.Severity != Warning || f.FixScript == "") {
			t.Errorf("%s: finding %+v", c.name, f)
		}
	}
}

func TestPreloadHasPgaudit(t *testing.T) {
	for in, want := range map[string]bool{"pgaudit": true, "a, pgaudit": true,
		"'pgaudit'": true, "pgaudit_x": false, "": false, "pg_stat_statements": false,
		"\"$libdir/pgaudit\"": true} {
		if got := preloadHasPgaudit(in); got != want {
			t.Errorf("preloadHasPgaudit(%q) = %v, want %v", in, got, want)
		}
	}
}
