package config

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Sage SRE M7 config (sre.autonomy.*): the earned-autonomy ledger is
// enforced by default; game days are off by default; intervals have
// validated defaults that an absent file, a partial section or explicit
// zeros cannot mask. Promotion thresholds are the spec's by default and
// configurable only under sre.autonomy.promotion (elevation_config_test.go).

func TestSREAutonomyDefaults_NoConfigFile(t *testing.T) {
	chdirTemp(t)
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.SRE.Autonomy
	if !a.Enforce || a.BenchResultsPath != "" || a.EvaluateIntervalMinutes != 60 ||
		a.ReconcileIntervalSeconds != 60 || a.MaxEvidenceAgeSeconds != 300 ||
		a.ConcurrencyWindowMinutes != 15 || a.SafetyWindowDays != 30 ||
		a.FailoverCooldownMinutes != 30 || a.ProposalTTLHours != 168 {
		t.Fatalf("autonomy defaults = %+v", a)
	}
	g := a.GameDays
	if g.Enabled || g.IntervalHours != 168 || g.LocalDSN != "" || len(g.Families) != 0 {
		t.Fatalf("game day defaults = %+v", g)
	}
	c := a.Canary
	if c.CanaryInstances != 1 || c.RegressionLimitPct != 10 || c.SettleSeconds != 60 {
		t.Fatalf("canary defaults = %+v", c)
	}
}

func TestSREAutonomy_PartialSectionKeepsTheRest(t *testing.T) {
	cfg, err := loadRCAYAML(t, "sre:\n  autonomy:\n    enforce: false\n"+
		"    game_days:\n      enabled: true\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.SRE.Autonomy
	if a.Enforce || !a.GameDays.Enabled || a.GameDays.IntervalHours != 168 ||
		a.SafetyWindowDays != 30 || cfg.SRE.TriggerIntervalSeconds != 15 {
		t.Fatalf("autonomy = %+v", a)
	}
}

func TestSREAutonomy_Boundaries(t *testing.T) {
	cases := []struct {
		key    string
		value  int
		wantOK bool
	}{
		{"evaluate_interval_minutes", 4, false}, {"evaluate_interval_minutes", 5, true},
		{"evaluate_interval_minutes", 1440, true}, {"evaluate_interval_minutes", 1441, false},
		{"reconcile_interval_seconds", 9, false}, {"reconcile_interval_seconds", 10, true},
		{"reconcile_interval_seconds", 3600, true}, {"reconcile_interval_seconds", 3601, false},
		{"max_evidence_age_seconds", 0, false}, {"max_evidence_age_seconds", 5, true},
		{"max_evidence_age_seconds", 3600, true}, {"max_evidence_age_seconds", 3601, false},
		{"concurrency_window_minutes", 0, false}, {"concurrency_window_minutes", 1, true},
		{"concurrency_window_minutes", 1440, true},
		{"concurrency_window_minutes", 1441, false},
		{"safety_window_days", 0, false}, {"safety_window_days", 1, true},
		{"safety_window_days", 365, true}, {"safety_window_days", 366, false},
		{"failover_cooldown_minutes", 0, false}, {"failover_cooldown_minutes", 1, true},
		{"failover_cooldown_minutes", 1441, false},
		{"proposal_ttl_hours", 0, false}, {"proposal_ttl_hours", 1, true},
		{"proposal_ttl_hours", 720, true}, {"proposal_ttl_hours", 721, false},
	}
	for _, c := range cases {
		t.Run(c.key+"="+strconv.Itoa(c.value), func(t *testing.T) {
			yaml := "sre:\n  autonomy:\n    " + c.key + ": " + strconv.Itoa(c.value) + "\n"
			_, err := loadRCAYAML(t, yaml)
			if c.wantOK && err != nil {
				t.Fatalf("%s=%d rejected: %v", c.key, c.value, err)
			}
			if !c.wantOK && (err == nil || !strings.Contains(err.Error(),
				"sre.autonomy."+c.key)) {
				t.Fatalf("%s=%d: err = %v, want one naming the key", c.key, c.value, err)
			}
		})
	}
}

func TestSREAutonomy_GameDayAndCanaryBoundaries(t *testing.T) {
	cases := []struct {
		yaml, key string
		wantOK    bool
	}{
		{"game_days:\n      interval_hours: 23", "game_days.interval_hours", false},
		{"game_days:\n      interval_hours: 24", "", true},
		{"game_days:\n      interval_hours: 2161", "game_days.interval_hours", false},
		{"game_days:\n      families: [lock_blocking, wal_retention]", "", true},
		{"game_days:\n      families: [\"DROP TABLE\"]", "game_days.families", false},
		{"game_days:\n      local_dsn: \"postgres://u@127.0.0.1:5999/scratch\"", "", true},
		{"game_days:\n      local_dsn: \"::not a dsn::\"", "game_days.local_dsn", false},
		{"canary:\n      canary_instances: 0", "canary.canary_instances", false},
		{"canary:\n      canary_instances: 10", "", true},
		{"canary:\n      canary_instances: 11", "canary.canary_instances", false},
		{"canary:\n      regression_limit_pct: -1", "canary.regression_limit_pct", false},
		{"canary:\n      regression_limit_pct: 100", "", true},
		{"canary:\n      regression_limit_pct: 101", "canary.regression_limit_pct", false},
		{"canary:\n      settle_seconds: -1", "canary.settle_seconds", false},
		{"canary:\n      settle_seconds: 3601", "canary.settle_seconds", false},
	}
	for _, c := range cases {
		_, err := loadRCAYAML(t, "sre:\n  autonomy:\n    "+c.yaml+"\n")
		if c.wantOK && err != nil {
			t.Errorf("%q rejected: %v", c.yaml, err)
		}
		if !c.wantOK && (err == nil || !strings.Contains(err.Error(), "sre.autonomy."+c.key)) {
			t.Errorf("%q: err = %v, want one naming %s", c.yaml, err, c.key)
		}
	}
}

func TestSREAutonomy_UnknownKeyRejected(t *testing.T) {
	if _, err := loadRCAYAML(t, "sre:\n  autonomy:\n    enforced: true\n"); err == nil {
		t.Fatal("a misspelled sre.autonomy key was accepted")
	}
	if _, err := loadRCAYAML(t, "sre:\n  autonomy:\n    min_top1: 0.5\n"); err == nil {
		t.Fatal("a promotion threshold outside sre.autonomy.promotion was accepted")
	}
}

func TestSREAutonomy_LocalDSNIsSecret(t *testing.T) {
	field, ok := reflect.TypeOf(SREGameDaysConfig{}).FieldByName("LocalDSN")
	if !ok || field.Tag.Get("secret") != "true" {
		t.Fatalf("game_days.local_dsn must be a secret field: %v", field.Tag)
	}
}
