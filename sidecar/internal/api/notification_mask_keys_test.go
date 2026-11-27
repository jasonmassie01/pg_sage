package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/notify"
)

// market-14: the API masks every channel secret the store seals, so a
// new destination's credential (or a webhook's headers) is never read
// back in full.
// No concurrent access tests: maskChannelSecrets edits one local map.

func TestMaskChannelSecrets_MasksEverySealedKey(t *testing.T) {
	cfg := map[string]string{"region": "eu", "alert_source_config_id": "01HXYZ"}
	for _, k := range notify.SecretConfigKeys {
		cfg[k] = "secret-value-" + k
	}
	maskChannelSecrets(cfg)
	for _, k := range notify.SecretConfigKeys {
		if cfg[k] == "secret-value-"+k {
			t.Errorf("secret %q returned unmasked", k)
		}
	}
	if cfg["region"] != "eu" || cfg["alert_source_config_id"] != "01HXYZ" {
		t.Fatalf("non-secret values were masked: %v", cfg)
	}
}
