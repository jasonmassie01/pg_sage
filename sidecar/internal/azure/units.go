package azure

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// postgresUnitBytes are PostgreSQL's memory units (1024-based, as in
// ALTER SYSTEM values such as '64MB').
var postgresUnitBytes = map[string]uint64{
	"":   1,
	"kb": 1 << 10,
	"mb": 1 << 20,
	"gb": 1 << 30,
	"tb": 1 << 40,
}

var (
	valuePattern     = regexp.MustCompile(`^([0-9]+)([kKmMgGtT][bB])?$`)
	azureUnitPattern = regexp.MustCompile(`^([0-9]*)\s*(B|KB|MB|GB|TB|bytes)$`)
)

// convertToUnit expresses a PostgreSQL memory value in the unit Azure
// reports for the parameter ("8KB" for shared_buffers, "KB" for work_mem).
// A value without a unit is already in the parameter's unit. A unitless
// parameter only accepts unitless values.
func convertToUnit(value, unit string) (string, error) {
	match := valuePattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return "", fmt.Errorf("invalid value %q", value)
	}
	amount, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil || amount == 0 {
		return "", fmt.Errorf("invalid value %q: must be a positive integer", value)
	}
	suffix := strings.ToLower(match[2])
	if suffix == "" {
		return match[1], nil
	}
	unitBytes, err := azureUnitBytes(unit)
	if err != nil {
		return "", err
	}
	if unitBytes == 0 {
		return "", fmt.Errorf("value %q has a unit but the parameter is unitless", value)
	}
	bytes := amount * postgresUnitBytes[suffix]
	if bytes%unitBytes != 0 {
		return "", fmt.Errorf("value %q is not a multiple of the parameter unit %s", value, unit)
	}
	return strconv.FormatUint(bytes/unitBytes, 10), nil
}

// azureUnitBytes parses an Azure unit such as "8KB", "KB", "MB" or "bytes".
// It returns 0 for a unitless parameter.
func azureUnitBytes(unit string) (uint64, error) {
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return 0, nil
	}
	match := azureUnitPattern.FindStringSubmatch(unit)
	if match == nil {
		return 0, fmt.Errorf("unsupported parameter unit %q", unit)
	}
	multiplier := uint64(1)
	if match[1] != "" {
		parsed, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil || parsed == 0 {
			return 0, fmt.Errorf("unsupported parameter unit %q", unit)
		}
		multiplier = parsed
	}
	base := map[string]uint64{"B": 1, "bytes": 1, "KB": 1 << 10, "MB": 1 << 20,
		"GB": 1 << 30, "TB": 1 << 40}[match[2]]
	return multiplier * base, nil
}

// ServerNameFromHost returns the flexible server name from its hostname
// (<name>.postgres.database.azure.com, optionally via privatelink), or "".
func ServerNameFromHost(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, suffix := range []string{
		".privatelink.postgres.database.azure.com", ".postgres.database.azure.com",
	} {
		if name, ok := strings.CutSuffix(host, suffix); ok && armNamePattern.MatchString(name) &&
			!strings.Contains(name, ".") {
			return name
		}
	}
	return ""
}
