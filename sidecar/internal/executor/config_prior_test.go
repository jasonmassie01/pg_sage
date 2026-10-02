package executor

import (
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// No concurrent access tests: rollback construction is pure; the live
// capture is covered by config_roundtrip_integration_test.go.

func TestGUCRollbackSQL(t *testing.T) {
	cases := []struct {
		name  string
		prior settingRow
		want  string
	}{
		{"work_mem", settingRow{Setting: "4096", Unit: "kB", Source: "default"},
			"ALTER SYSTEM RESET work_mem"},
		// wal_buffers=-1 resolves to a computed value; RESET restores auto.
		{"wal_buffers", settingRow{Setting: "512", Unit: "8kB", Source: "override"},
			"ALTER SYSTEM RESET wal_buffers"},
		{"work_mem", settingRow{Setting: "4096", Source: "environment variable"},
			"ALTER SYSTEM RESET work_mem"},
		// Set in postgresql.conf: removing our auto.conf entry restores it.
		{"shared_buffers", settingRow{Setting: "16384", Unit: "8kB",
			Source: "configuration file", SourceFile: "/data/postgresql.conf"},
			"ALTER SYSTEM RESET shared_buffers"},
		// Already in postgresql.auto.conf: restore that exact value.
		{"work_mem", settingRow{Setting: "8192", Unit: "kB",
			Source: "configuration file", SourceFile: "/data/postgresql.auto.conf"},
			"ALTER SYSTEM SET work_mem = '8192'"},
		// File not visible (no superuser): pin the prior effective value.
		{"random_page_cost", settingRow{Setting: "1.1", Source: "configuration file"},
			"ALTER SYSTEM SET random_page_cost = '1.1'"},
		{"jit", settingRow{Setting: "o'n", Source: "configuration file"},
			"ALTER SYSTEM SET jit = 'o''n'"},
	}
	for _, c := range cases {
		got, err := gucRollbackSQL(c.name, c.prior)
		if err != nil || got != c.want {
			t.Errorf("gucRollbackSQL(%s, %+v) = %q,%v want %q", c.name, c.prior, got, err, c.want)
		}
		if err == nil {
			if verr := ValidateExecutorSQL(got); verr != nil &&
				pgconf.ExecutableGUC(c.name) {
				t.Errorf("rollback %q fails executor validation: %v", got, verr)
			}
		}
	}
}

func TestGUCRollbackSQLRefusals(t *testing.T) {
	cases := []struct {
		prior settingRow
		want  error
	}{
		{settingRow{Setting: "100", Source: "command line"}, ErrConfigOverridden},
		{settingRow{Setting: "8192", Source: "database"}, ErrConfigOverridden},
		{settingRow{Setting: "8192", Source: "user"}, ErrConfigOverridden},
		{settingRow{Setting: "8192", Source: "database user"}, ErrConfigOverridden},
		{settingRow{Setting: "8192", Source: "client"}, ErrConfigOverridden},
		{settingRow{Setting: "8192", Source: "session"}, ErrConfigOverridden},
		{settingRow{Setting: "8192", Source: ""}, ErrConfigOverridden},
		{settingRow{Setting: "16384", Source: "configuration file",
			PendingRestart: true}, ErrConfigPendingChange},
	}
	for _, c := range cases {
		got, err := gucRollbackSQL("work_mem", c.prior)
		if !errors.Is(err, c.want) || got != "" {
			t.Errorf("gucRollbackSQL(%+v) = %q,%v want error %v", c.prior, got, err, c.want)
		}
	}
}

func TestReloptionRollbackSQL(t *testing.T) {
	set := func(sql string) pgconf.TableStmt {
		stmt, ok := pgconf.ParseAlterTableReloptions(sql)
		if !ok {
			t.Fatalf("parse %q", sql)
		}
		return stmt
	}
	none := map[string]string{}
	cases := []struct {
		stmt        pgconf.TableStmt
		main, toast map[string]string
		want        string
	}{
		{set(`ALTER TABLE "public"."t" SET (autovacuum_vacuum_scale_factor = 0.02)`),
			none, none, `ALTER TABLE "public"."t" RESET (autovacuum_vacuum_scale_factor)`},
		{set(`ALTER TABLE public.t SET (fillfactor = 90)`),
			map[string]string{"fillfactor": "80"}, none,
			`ALTER TABLE public.t SET (fillfactor = 80)`},
		{set(`ALTER TABLE public.t SET (fillfactor = 90, autovacuum_vacuum_threshold = 9)`),
			map[string]string{"fillfactor": "80", "autovacuum_vacuum_threshold": "500"}, none,
			`ALTER TABLE public.t SET (fillfactor = 80, autovacuum_vacuum_threshold = 500)`},
		{set(`ALTER TABLE public.t SET (toast.autovacuum_vacuum_threshold = 9)`),
			map[string]string{"autovacuum_vacuum_threshold": "1"},
			map[string]string{"autovacuum_vacuum_threshold": "700"},
			`ALTER TABLE public.t SET (toast.autovacuum_vacuum_threshold = 700)`},
		{set(`ALTER TABLE public.t SET (toast.autovacuum_vacuum_threshold = 9)`),
			map[string]string{"autovacuum_vacuum_threshold": "1"}, none,
			`ALTER TABLE public.t RESET (toast.autovacuum_vacuum_threshold)`},
		// A forward RESET restores the prior explicit value.
		{set(`ALTER TABLE public.t RESET (fillfactor)`),
			map[string]string{"fillfactor": "70"}, none,
			`ALTER TABLE public.t SET (fillfactor = 70)`},
		{set(`ALTER TABLE public.t RESET (fillfactor)`), none, none,
			`ALTER TABLE public.t RESET (fillfactor)`},
	}
	for _, c := range cases {
		got, err := reloptionRollbackSQL(c.stmt, c.main, c.toast)
		if err != nil || got != c.want {
			t.Errorf("reloptionRollbackSQL(%+v) = %q,%v want %q", c.stmt, got, err, c.want)
			continue
		}
		if verr := ValidateExecutorSQL(got); verr != nil {
			t.Errorf("rollback %q fails executor validation: %v", got, verr)
		}
	}
}

// One key set before and one not: a single statement cannot restore both
// (SET and RESET would be two subcommands), so the change is refused.
func TestReloptionRollbackSQLMixedPriorRefused(t *testing.T) {
	stmt, _ := pgconf.ParseAlterTableReloptions(
		`ALTER TABLE public.t SET (fillfactor = 90, autovacuum_vacuum_threshold = 9)`)
	got, err := reloptionRollbackSQL(stmt, map[string]string{"fillfactor": "80"}, nil)
	if !errors.Is(err, ErrNoFaithfulRollback) || got != "" {
		t.Errorf("mixed prior = %q,%v want ErrNoFaithfulRollback", got, err)
	}
}

func TestReloptionLiteral(t *testing.T) {
	cases := map[string]string{"80": "80", "0.02": "0.02", "-1": "-1", "true": "true",
		"a b": "'a b'", "x'y": "'x''y'", "": "''"}
	for in, want := range cases {
		if got := reloptionLiteral(in); got != want {
			t.Errorf("reloptionLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseReloptionArray(t *testing.T) {
	got := parseReloptionArray([]string{"fillfactor=80", "autovacuum_enabled=false", "bad"})
	if len(got) != 2 || got["fillfactor"] != "80" || got["autovacuum_enabled"] != "false" {
		t.Errorf("parseReloptionArray = %v", got)
	}
	if got := parseReloptionArray(nil); got == nil || len(got) != 0 {
		t.Errorf("nil reloptions = %v, want empty map", got)
	}
}
