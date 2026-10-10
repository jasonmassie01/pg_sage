package decommission

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// deterministicName reproduces the name the removed provisioner gave a
// provider resource for a deployment (ProviderResourceName at 62d27f6e), so
// an uncertain create with no recorded id can still be found. It is empty
// for local and unknown providers.
func deterministicName(provider, deploymentID string) string {
	if strings.TrimSpace(deploymentID) == "" {
		return ""
	}
	base := resourceName(deploymentID)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case providerNeon, providerSupabase:
		if len(base) > 46 {
			base = base[:46]
		}
		return dnsLabel("pgsage-" + base + "-" + nameDigest(deploymentID))
	case providerRDS, providerCloudSQL, providerLakebase:
		return dnsLabel("pgsage-" + base)
	}
	return ""
}

// nameDigest is the first four bytes of the id's SHA-256, in hex.
func nameDigest(deploymentID string) string {
	sum := sha256.Sum256([]byte(deploymentID))
	return hex.EncodeToString(sum[:4])
}

func resourceName(value string) string {
	value = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-")
	out := collapse(value, func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	})
	if out == "" {
		return "agentdb"
	}
	return out
}

func dnsLabel(value string) string {
	out := collapse(strings.ToLower(strings.TrimSpace(value)), func(r rune) bool {
		return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
	})
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

// collapse keeps the runes keep accepts and turns every run of other runes
// into one dash, trimming dashes at both ends.
func collapse(value string, keep func(rune) bool) string {
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		if keep(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
