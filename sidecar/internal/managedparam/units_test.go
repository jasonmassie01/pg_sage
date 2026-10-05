package managedparam

import (
	"errors"
	"testing"
)

// ProviderValue converts a PostgreSQL setting value into the unit RDS
// parameter groups and Cloud SQL flags use: the parameter's base unit
// (pg_settings.unit), with booleans in the provider's spelling.
func TestProviderValue(t *testing.T) {
	cases := []struct {
		provider, parameter, value string
		want, unit                 string
	}{
		{"rds", "shared_buffers", "4GB", "524288", "8kB"},
		{"rds", "shared_buffers", "524288", "524288", "8kB"},
		{"rds", "work_mem", "64MB", "65536", "kB"},
		{"rds", "maintenance_work_mem", "1GB", "1048576", "kB"},
		{"rds", "max_wal_size", "4GB", "4096", "MB"},
		{"rds", "autovacuum_naptime", "1min", "60", "s"},
		{"rds", "checkpoint_timeout", "15min", "900", "s"},
		{"rds", "autovacuum_vacuum_cost_delay", "2ms", "2", "ms"},
		{"rds", "random_page_cost", "1.1", "1.1", ""},
		{"rds", "max_connections", "500", "500", ""},
		{"rds", "jit", "off", "0", ""},
		{"rds", "jit", "on", "1", ""},
		{"cloud-sql", "jit", "true", "on", ""},
		{"cloud-sql", "jit", "0", "off", ""},
		{"cloud-sql", "work_mem", "128MB", "131072", "kB"},
		{"rds", "wal_level", "logical", "logical", ""},
	}
	for _, tc := range cases {
		got, unit, err := ProviderValue(tc.provider, tc.parameter, tc.value)
		if err != nil || got != tc.want || unit != tc.unit {
			t.Errorf("ProviderValue(%s, %s, %s) = %q %q %v; want %q %q", tc.provider,
				tc.parameter, tc.value, got, unit, err, tc.want, tc.unit)
		}
	}
}

func TestProviderValueRejects(t *testing.T) {
	cases := []struct{ parameter, value string }{
		{"work_mem", "4mb"},             // units are case-sensitive in PostgreSQL
		{"shared_buffers", "4097B"},     // not a whole number of 8kB pages
		{"work_mem", "-1MB"},            // negative
		{"work_mem", "99999999TB"},      // overflows the parameter range
		{"unknown_param", "30s"},        // a unit on a parameter without a known base unit
		{"work_mem", ""},                // empty
		{"work_mem", "1 MB"},            // whitespace
		{"jit", "maybe"},                // not a boolean or keyword we pass through
		{"autovacuum_naptime", "1.5us"}, // below the base unit
	}
	for _, tc := range cases {
		if got, _, err := ProviderValue("rds", tc.parameter, tc.value); !errors.Is(err,
			ErrInvalidValue) {
			t.Errorf("ProviderValue(%s, %q) = %q, %v; want ErrInvalidValue", tc.parameter,
				tc.value, got, err)
		}
	}
}
