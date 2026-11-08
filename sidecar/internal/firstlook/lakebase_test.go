package firstlook

import (
	"strings"
	"testing"
)

func TestLakebaseCapabilitiesDoNotOfferHostAdministration(t *testing.T) {
	// Lakebase can preload auto_explain while keeping server logs unavailable.
	caps, _ := ExtensionCapabilities(Extensions{AutoExplainLoaded: true}, "lakebase")
	for _, name := range []string{CapHypoPG, CapAutoExplain} {
		c := capability(caps, name)
		if c.Status != CapabilityUnavailable {
			t.Errorf("%s status=%s; want unavailable", name, c.Status)
		}
	}
	for _, c := range caps {
		steps := strings.Join(c.Steps, " ")
		if strings.Contains(steps, "ALTER SYSTEM") || strings.Contains(steps, "apt install") {
			t.Fatalf("Lakebase offered unsupported host administration: %s", steps)
		}
	}
}
