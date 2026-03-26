package advisor

import (
	"fmt"
	"strconv"
	"strings"
)

// Known managed service platform restrictions.
var restrictedSettings = map[string]map[string]bool{
	"cloud-sql": {
		"wal_level": true, "full_page_writes": true, "shared_buffers": true,
	},
	"alloydb": {
		"wal_level": true, "full_page_writes": true, "shared_buffers": true,
	},
	"aurora": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
	},
	"rds": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
	},
}

// dangerousLimits defines min/max values for safety.
var dangerousLimits = map[string][2]float64{
	"max_connections":                {10, 10000},
	"autovacuum_vacuum_scale_factor": {0.001, 1.0},
	"autovacuum_vacuum_threshold":    {0, 1000000},
	"autovacuum_vacuum_cost_delay":   {0, 100},
	"autovacuum_vacuum_cost_limit":   {1, 10000},
	"work_mem":                       {1, 1048576}, // 1KB to 1GB in KB
}

// restartRequired lists GUCs that need a restart.
var restartRequired = map[string]bool{
	"max_connections": true,
	"shared_buffers":  true,
	"huge_pages":      true,
	"wal_level":       true,
	"max_wal_senders": true,
	"wal_buffers":     true,
}

// ValidateConfigRecommendation checks a recommended setting change.
func ValidateConfigRecommendation(
	settingName, value, platform string,
) error {
	if settingName == "" {
		return fmt.Errorf("empty setting name")
	}

	// Check managed service restrictions.
	if platform != "" {
		if restricted, ok := restrictedSettings[platform]; ok {
			if restricted[settingName] {
				return fmt.Errorf(
					"%s not adjustable on %s", settingName, platform,
				)
			}
		}
	}

	// Check dangerous values.
	if limits, ok := dangerousLimits[settingName]; ok {
		numVal, err := parseNumericValue(value)
		if err == nil {
			if numVal < limits[0] || numVal > limits[1] {
				return fmt.Errorf(
					"%s=%s out of safe range [%.0f, %.0f]",
					settingName, value, limits[0], limits[1],
				)
			}
		}
	}

	return nil
}

// RequiresRestart returns true if changing the setting needs a restart.
func RequiresRestart(settingName string) bool {
	return restartRequired[settingName]
}

func parseNumericValue(s string) (float64, error) {
	s = strings.TrimSpace(s)
	// Strip common PG units
	for _, suffix := range []string{"MB", "GB", "kB", "ms", "s", "min"} {
		s = strings.TrimSuffix(s, suffix)
	}
	s = strings.TrimSpace(s)
	return strconv.ParseFloat(s, 64)
}
