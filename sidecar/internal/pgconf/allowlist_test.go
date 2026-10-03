package pgconf

import (
	"errors"
	"testing"
)

// No concurrent access tests: the allowlists are read-only package maps.

func TestRequiresRestart(t *testing.T) {
	restart := []string{
		"shared_buffers", "max_connections", "wal_buffers", "huge_pages",
		"wal_level", "max_wal_senders", "max_worker_processes",
		"max_prepared_transactions", "max_replication_slots",
		"shared_preload_libraries", "max_locks_per_transaction",
		"superuser_reserved_connections",
		// Postmaster context before PG18 (G-P0-1): a reload never applies it.
		"autovacuum_max_workers",
		"Shared_Buffers", " max_connections ",
	}
	for _, name := range restart {
		if !RequiresRestart(name) {
			t.Errorf("RequiresRestart(%q) = false, want true", name)
		}
	}
	reload := []string{"work_mem", "random_page_cost", "max_wal_size",
		"autovacuum_vacuum_scale_factor", "checkpoint_timeout", "", "no_such_guc"}
	for _, name := range reload {
		if RequiresRestart(name) {
			t.Errorf("RequiresRestart(%q) = true, want false", name)
		}
	}
}

func TestAdvisorAndAutonomousGUCs(t *testing.T) {
	cases := []struct {
		name                 string
		advisor, autonomous  bool
	}{
		{"work_mem", true, true},
		{"maintenance_work_mem", true, true},
		{"random_page_cost", true, true},
		{"checkpoint_timeout", true, true},
		{"min_wal_size", true, true},
		{"autovacuum_analyze_scale_factor", true, true},
		// Documented with a range but restart-required: approval only.
		{"shared_buffers", true, false},
		{"max_connections", true, false},
		{"wal_buffers", true, false},
		// Not on the list at all: advisory only.
		{"log_statement", false, false},
		{"idle_in_transaction_session_timeout", false, false},
		{"fsync", false, false},
		{"", false, false},
		{"WORK_MEM", true, true},
	}
	for _, c := range cases {
		if got := AdvisorGUC(c.name); got != c.advisor {
			t.Errorf("AdvisorGUC(%q) = %v, want %v", c.name, got, c.advisor)
		}
		if got := AutonomousGUC(c.name); got != c.autonomous {
			t.Errorf("AutonomousGUC(%q) = %v, want %v", c.name, got, c.autonomous)
		}
	}
}

// Every GUC the advisor may propose must also pass the executor's
// whitelist, or an approved advisor finding could never run.
func TestAdvisorGUCsAreExecutable(t *testing.T) {
	for name := range Docs {
		if !ExecutableGUC(name) {
			t.Errorf("advisor GUC %q is not executable", name)
		}
	}
	for _, name := range []string{"fsync", "listen_addresses", "log_statement", ""} {
		if ExecutableGUC(name) {
			t.Errorf("ExecutableGUC(%q) = true, want false", name)
		}
	}
	if !ExecutableGUC("max_slot_wal_keep_size") {
		t.Error("custodian WAL bound must stay executable")
	}
}

func TestValidateValue(t *testing.T) {
	cases := []struct {
		name, value string
		ok          bool
	}{
		{"work_mem", "64MB", true},
		{"work_mem", "8GB", false},
		{"checkpoint_timeout", "900s", true},
		{"checkpoint_timeout", "900", true}, // base unit is seconds
		{"checkpoint_timeout", "15min", true},
		{"checkpoint_timeout", "1d", false},
		{"checkpoint_timeout", "10s", false},
		{"min_wal_size", "4GB", true},
		{"min_wal_size", "1MB", false},
		{"autovacuum_analyze_scale_factor", "0.05", true},
		{"autovacuum_analyze_scale_factor", "0", false},
		{"unknown_guc", "whatever", true}, // range check only knows documented GUCs
		{"work_mem", "lots", false},
	}
	for _, c := range cases {
		ok, reason := ValidateValue(c.name, c.value)
		if ok != c.ok {
			t.Errorf("ValidateValue(%s=%q) = %v (%s), want %v", c.name, c.value, ok, reason, c.ok)
		}
		if !ok && reason == "" {
			t.Errorf("ValidateValue(%s=%q) refused without a reason", c.name, c.value)
		}
	}
}

func TestAdvisorReloption(t *testing.T) {
	allowed := []string{
		"autovacuum_vacuum_scale_factor", "autovacuum_vacuum_threshold",
		"autovacuum_analyze_scale_factor", "autovacuum_analyze_threshold",
		"autovacuum_vacuum_insert_scale_factor", "autovacuum_vacuum_insert_threshold",
		"autovacuum_vacuum_cost_limit", "autovacuum_vacuum_cost_delay", "fillfactor",
		"toast.autovacuum_vacuum_scale_factor", "toast.autovacuum_vacuum_cost_limit",
		"FILLFACTOR",
	}
	for _, key := range allowed {
		if !AdvisorReloption(key) {
			t.Errorf("AdvisorReloption(%q) = false, want true", key)
		}
	}
	refused := []string{
		"autovacuum_enabled", "toast.autovacuum_enabled", "parallel_workers",
		"user_catalog_table", "vacuum_truncate", "toast.fillfactor",
		"toast.autovacuum_analyze_scale_factor", "", "toast.",
	}
	for _, key := range refused {
		if AdvisorReloption(key) {
			t.Errorf("AdvisorReloption(%q) = true, want false", key)
		}
	}
}

func TestValidateReloption(t *testing.T) {
	cases := []struct {
		key, value string
		ok         bool
	}{
		{"fillfactor", "90", true},
		{"fillfactor", "100", true},
		{"fillfactor", "50", true},
		{"fillfactor", "49", false},
		{"fillfactor", "101", false},
		{"autovacuum_vacuum_scale_factor", "0.02", true},
		{"autovacuum_vacuum_scale_factor", "0", false},
		{"toast.autovacuum_vacuum_scale_factor", "0", false},
		{"autovacuum_vacuum_cost_limit", "-1", true},
		{"autovacuum_vacuum_insert_threshold", "-1", true},
		{"autovacuum_vacuum_threshold", "abc", false},
	}
	for _, c := range cases {
		ok, reason := ValidateReloption(c.key, c.value)
		if ok != c.ok {
			t.Errorf("ValidateReloption(%s=%q) = %v (%s), want %v",
				c.key, c.value, ok, reason, c.ok)
		}
	}
}

func TestCheckExecutableReloption(t *testing.T) {
	refused := []Reloption{
		{Key: "autovacuum_enabled", Value: "false"},
		{Key: "autovacuum_enabled", Value: "off"},
		{Key: "autovacuum_enabled", Value: "0"},
		{Key: "autovacuum_enabled", Value: "no"},
		{Key: "autovacuum_enabled", Value: "FALSE"},
		{Key: "autovacuum_enabled", Value: "f"},
		{Key: "autovacuum_enabled", Value: "garbage"},
		{Key: "toast.autovacuum_enabled", Value: "false"},
		{Key: "toast.autovacuum_enabled", Value: "of"},
		{Key: "parallel_workers", Value: "4"},
		{Key: "user_catalog_table", Value: "true"},
		{Key: "log_autovacuum_min_duration", Value: "0"},
		{Key: "", Value: "1"},
	}
	for _, opt := range refused {
		err := CheckExecutableReloption(opt, false)
		if !errors.Is(err, ErrReloptionRefused) {
			t.Errorf("CheckExecutableReloption(%+v) = %v, want ErrReloptionRefused", opt, err)
		}
	}
	allowed := []Reloption{
		{Key: "autovacuum_enabled", Value: "true"},
		{Key: "autovacuum_enabled", Value: "on"},
		{Key: "toast.autovacuum_enabled", Value: "yes"},
		{Key: "fillfactor", Value: "90"},
		{Key: "autovacuum_vacuum_scale_factor", Value: "0.02"},
		{Key: "toast.autovacuum_vacuum_threshold", Value: "1000"},
		{Key: "autovacuum_freeze_max_age", Value: "100000000"},
	}
	for _, opt := range allowed {
		if err := CheckExecutableReloption(opt, false); err != nil {
			t.Errorf("CheckExecutableReloption(%+v) = %v, want nil", opt, err)
		}
	}
	// RESET restores the default (autovacuum on), so it is always allowed
	// for a known key, and never for an unknown one.
	if err := CheckExecutableReloption(Reloption{Key: "autovacuum_enabled"}, true); err != nil {
		t.Errorf("RESET (autovacuum_enabled) refused: %v", err)
	}
	if err := CheckExecutableReloption(Reloption{Key: "parallel_workers"}, true); err == nil {
		t.Error("RESET (parallel_workers) accepted, want refusal")
	}
}
