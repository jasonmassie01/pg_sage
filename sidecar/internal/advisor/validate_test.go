package advisor

import (
	"strings"
	"testing"
)

func TestConfigValidate_ValidGUC(t *testing.T) {
	err := ValidateConfigRecommendation("max_wal_size", "4GB", "")
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestConfigValidate_InvalidGUC_EmptyName(t *testing.T) {
	err := ValidateConfigRecommendation("", "4GB", "")
	if err == nil {
		t.Fatal("expected error for empty setting name")
	}
	if !strings.Contains(err.Error(), "empty setting name") {
		t.Fatalf("expected 'empty setting name' in error, got: %v", err)
	}
}

func TestConfigValidate_ValueInRange(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_scale_factor", "0.5", "",
	)
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
}

func TestConfigValidate_ValueOutOfRange(t *testing.T) {
	err := ValidateConfigRecommendation(
		"autovacuum_vacuum_scale_factor", "5.0", "",
	)
	if err == nil {
		t.Fatal("expected error for out-of-range value")
	}
	if !strings.Contains(err.Error(), "out of safe range") {
		t.Fatalf("expected 'out of safe range' in error, got: %v", err)
	}
}

func TestConfigValidate_ValueBelowRange(t *testing.T) {
	err := ValidateConfigRecommendation("max_connections", "5", "")
	if err == nil {
		t.Fatal("expected error for below-range value")
	}
}

func TestConfigValidate_ManagedRestriction_CloudSQL(t *testing.T) {
	err := ValidateConfigRecommendation("wal_level", "replica", "cloud-sql")
	if err == nil {
		t.Fatal("expected error for restricted setting on cloud-sql")
	}
	if !strings.Contains(err.Error(), "not adjustable on cloud-sql") {
		t.Fatalf(
			"expected 'not adjustable on cloud-sql' in error, got: %v", err,
		)
	}
}

func TestConfigValidate_ManagedRestriction_CloudSQL_Allowed(t *testing.T) {
	err := ValidateConfigRecommendation("work_mem", "64", "cloud-sql")
	if err != nil {
		t.Fatalf("expected nil error for non-restricted setting, got: %v", err)
	}
}

func TestConfigValidate_ManagedRestriction_AlloyDB(t *testing.T) {
	err := ValidateConfigRecommendation(
		"shared_buffers", "256MB", "alloydb",
	)
	if err == nil {
		t.Fatal("expected error for restricted setting on alloydb")
	}
}

func TestConfigValidate_ManagedRestriction_Aurora(t *testing.T) {
	err := ValidateConfigRecommendation("max_wal_size", "2GB", "aurora")
	if err == nil {
		t.Fatal("expected error for restricted setting on aurora")
	}
}

func TestConfigValidate_ManagedRestriction_RDS(t *testing.T) {
	err := ValidateConfigRecommendation("max_wal_size", "2GB", "rds")
	if err == nil {
		t.Fatal("expected error for restricted setting on rds")
	}
}

func TestConfigValidate_ManagedRestriction_SelfManaged(t *testing.T) {
	err := ValidateConfigRecommendation(
		"wal_level", "logical", "self-managed",
	)
	if err != nil {
		t.Fatalf("expected nil error for self-managed, got: %v", err)
	}
}

func TestConfigValidate_DangerousValue_MaxConnections(t *testing.T) {
	err := ValidateConfigRecommendation("max_connections", "5", "")
	if err == nil {
		t.Fatal("expected error for dangerous max_connections value")
	}
}

func TestConfigValidate_DangerousValue_WorkMem(t *testing.T) {
	err := ValidateConfigRecommendation("work_mem", "2000000", "")
	if err == nil {
		t.Fatal("expected error for dangerous work_mem value")
	}
}

func TestConfigValidate_ValueWithUnit(t *testing.T) {
	err := ValidateConfigRecommendation("max_wal_size", "4GB", "")
	if err != nil {
		t.Fatalf("expected nil error for value with unit suffix, got: %v", err)
	}
}

func TestRequiresRestart_MaxConnections(t *testing.T) {
	if !RequiresRestart("max_connections") {
		t.Fatal("expected max_connections to require restart")
	}
}

func TestRequiresRestart_SharedBuffers(t *testing.T) {
	if !RequiresRestart("shared_buffers") {
		t.Fatal("expected shared_buffers to require restart")
	}
}

func TestRequiresRestart_WorkMem(t *testing.T) {
	if RequiresRestart("work_mem") {
		t.Fatal("expected work_mem to not require restart")
	}
}

func TestRequiresRestart_WalBuffers(t *testing.T) {
	if !RequiresRestart("wal_buffers") {
		t.Fatal("expected wal_buffers to require restart")
	}
}

func TestRequiresRestart_Unknown(t *testing.T) {
	if RequiresRestart("nonexistent_setting") {
		t.Fatal("expected unknown setting to not require restart")
	}
}

func TestConfigValidate_ManagedServiceRestrictions_Complete(t *testing.T) {
	// Test that every restricted setting is rejected on its platform.
	for platform, settings := range restrictedSettings {
		for setting := range settings {
			t.Run(platform+"/"+setting+"/rejected", func(t *testing.T) {
				err := ValidateConfigRecommendation(
					setting, "1", platform,
				)
				if err == nil {
					t.Fatalf(
						"%s should be restricted on %s",
						setting, platform,
					)
				}
				if !strings.Contains(
					err.Error(), "not adjustable on "+platform,
				) {
					t.Fatalf("unexpected error message: %v", err)
				}
			})
		}
	}

	// Test that a non-restricted setting passes on each platform.
	nonRestricted := "maintenance_work_mem"
	for platform := range restrictedSettings {
		t.Run(platform+"/"+nonRestricted+"/allowed", func(t *testing.T) {
			err := ValidateConfigRecommendation(
				nonRestricted, "512MB", platform,
			)
			if err != nil {
				t.Fatalf(
					"%s should be allowed on %s, got: %v",
					nonRestricted, platform, err,
				)
			}
		})
	}
}
