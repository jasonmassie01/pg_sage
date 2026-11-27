package azure

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// convertToUnit expresses a PostgreSQL memory value in the unit Azure
// reports for the parameter (managedparam.AzureValue, shared with the
// server-parameter proposals so both convert identically).
func convertToUnit(value, unit string) (string, error) {
	return managedparam.AzureValue(value, unit)
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
