package advisor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// Known managed service platform restrictions.
var restrictedSettings = map[string]map[string]bool{
	"cloud-sql": {
		"wal_level": true, "full_page_writes": true,
		"shared_buffers": true, "checkpoint_timeout": true,
	},
	"alloydb": {
		"wal_level": true, "full_page_writes": true,
		"shared_buffers": true, "checkpoint_timeout": true,
	},
	"aurora": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
		"checkpoint_timeout": true,
	},
	"rds": {
		"wal_level": true, "full_page_writes": true,
		"max_wal_size": true, "min_wal_size": true,
		"checkpoint_timeout": true,
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

// IsManagedService returns true if the platform is a managed cloud
// service where ALTER SYSTEM is unavailable.
func IsManagedService(platform string) bool {
	switch platform {
	case "rds", "aurora", "cloud-sql", "alloydb", "azure":
		return true
	}
	return false
}

// TransformForCloud rewrites advisor findings for cloud platforms.
// On managed services: ALTER SYSTEM SET → ALTER DATABASE dbname SET,
// and restart-requiring GUCs are downgraded to info-only (no SQL).
func TransformForCloud(
	findings []analyzer.Finding,
	platform, dbName string,
) []analyzer.Finding {
	if !IsManagedService(platform) {
		return findings
	}

	out := make([]analyzer.Finding, 0, len(findings))
	for _, f := range findings {
		sql := strings.TrimSpace(f.RecommendedSQL)
		if sql == "" {
			out = append(out, f)
			continue
		}

		settingName := extractSettingName(sql)

		// Drop executable SQL for restart-requiring or
		// platform-restricted GUCs — managed services
		// control these via their console, not SQL.
		if settingName != "" && RequiresRestart(settingName) {
			f.RecommendedSQL = ""
			f.RollbackSQL = ""
			f.Recommendation += fmt.Sprintf(
				" (Note: %s requires a restart and must "+
					"be changed via %s console, not SQL.)",
				settingName, platform,
			)
			f.Severity = "info"
			out = append(out, f)
			continue
		}
		if settingName != "" {
			if restricted, ok := restrictedSettings[platform]; ok {
				if restricted[settingName] {
					f.RecommendedSQL = ""
					f.RollbackSQL = ""
					f.Recommendation += fmt.Sprintf(
						" (Note: %s is not adjustable via SQL "+
							"on %s — change via platform console.)",
						settingName, platform,
					)
					f.Severity = "info"
					out = append(out, f)
					continue
				}
			}
		}

		// Rewrite ALTER SYSTEM → ALTER DATABASE.
		upper := strings.ToUpper(sql)
		quoted := sanitize.QuoteIdentifier(dbName)
		if strings.HasPrefix(upper, "ALTER SYSTEM SET ") {
			rest := sql[len("ALTER SYSTEM SET "):]
			f.RecommendedSQL = fmt.Sprintf(
				"ALTER DATABASE %s SET %s", quoted, rest,
			)
			if settingName != "" {
				f.RollbackSQL = fmt.Sprintf(
					"ALTER DATABASE %s RESET %s",
					quoted, settingName,
				)
			}
		} else if strings.HasPrefix(upper, "ALTER SYSTEM RESET ") {
			rest := sql[len("ALTER SYSTEM RESET "):]
			f.RecommendedSQL = fmt.Sprintf(
				"ALTER DATABASE %s RESET %s", quoted, rest,
			)
		}

		out = append(out, f)
	}
	return out
}

// extractSettingName parses the GUC name from ALTER SYSTEM SET name
// or ALTER SYSTEM RESET name statements.
func extractSettingName(sql string) string {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	var rest string
	switch {
	case strings.HasPrefix(upper, "ALTER SYSTEM SET "):
		rest = strings.TrimSpace(sql[len("ALTER SYSTEM SET "):])
	case strings.HasPrefix(upper, "ALTER SYSTEM RESET "):
		rest = strings.TrimSpace(sql[len("ALTER SYSTEM RESET "):])
	default:
		return ""
	}
	// Setting name is the first token.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
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
