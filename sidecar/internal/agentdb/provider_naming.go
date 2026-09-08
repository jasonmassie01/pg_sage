package agentdb

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode"
)

func ProviderResourceName(provider string, deploymentID string) (string, error) {
	base := resourceName(deploymentID)
	switch normalizeProvider(provider) {
	case ProviderNeon, ProviderSupabase:
		if strings.TrimSpace(deploymentID) == "" {
			return "", ErrInvalid
		}
		digest := sha256.Sum256([]byte(deploymentID))
		if len(base) > 46 {
			base = base[:46]
		}
		return providerDNSLabel(fmt.Sprintf("pgsage-%s-%x", base, digest[:4])), nil
	case ProviderAWSRDS:
		return providerDNSLabel("pgsage-" + base), nil
	case ProviderGCPCloudSQL:
		return providerDNSLabel("pgsage-" + base), nil
	case ProviderDatabricksLakebase:
		return providerDNSLabel("pgsage-" + base), nil
	default:
		return "", ErrInvalid
	}
}

func providerDNSLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := unicode.IsLower(r) || unicode.IsDigit(r)
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "pgsage-agentdb"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "pgsage-" + out
	}
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}
