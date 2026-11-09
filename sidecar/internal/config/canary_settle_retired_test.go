package config

import (
	"strings"
	"testing"
)

// executor-04: a fleet canary waits for each database's own verification
// verdict, so sre.autonomy.canary.settle_seconds no longer has a reader.
// A config that sets it still loads, whatever its value, with one warning
// telling the operator to remove it; the rest of the block still loads.

const settleSecondsWarning = "sre.autonomy.canary.settle_seconds"

func TestCanarySettleSecondsLoadsWithOneWarning(t *testing.T) {
	for _, value := range []string{"60", "0", "-1", "3601", "\"soon\""} {
		cfg, out, err := loadWithWarnings(t, "sre:\n  autonomy:\n    canary:\n"+
			"      settle_seconds: "+value+"\n      canary_instances: 3\n")
		if err != nil {
			t.Fatalf("settle_seconds %s: Load: %v", value, err)
		}
		if got := strings.Count(out, settleSecondsWarning); got != 1 {
			t.Fatalf("settle_seconds %s: warnings %q mention it %d times, want once", value,
				out, got)
		}
		if !strings.Contains(out, "WARNING:") || !strings.Contains(out, "ignored") ||
			!strings.Contains(out, "verdict") {
			t.Fatalf("warning %q does not say the key is ignored and why", out)
		}
		if cfg.SRE.Autonomy.Canary.CanaryInstances != 3 {
			t.Fatalf("canary_instances = %d, want 3: the rest of the block must load",
				cfg.SRE.Autonomy.Canary.CanaryInstances)
		}
	}
}

func TestCanaryWithoutSettleSecondsHasNoWarning(t *testing.T) {
	_, out, err := loadWithWarnings(t, "sre:\n  autonomy:\n    canary:\n"+
		"      canary_instances: 2\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(out, settleSecondsWarning) {
		t.Fatalf("warnings %q mention settle_seconds, which is not set", out)
	}
}

// A settle_seconds key outside the canary block is not the retired key:
// it stays an unknown key, which the strict decode rejects.
func TestSettleSecondsElsewhereIsStillUnknown(t *testing.T) {
	_, out, err := loadWithWarnings(t, "sre:\n  autonomy:\n    settle_seconds: 60\n")
	if err == nil {
		t.Fatal("sre.autonomy.settle_seconds was accepted")
	}
	if strings.Contains(out, settleSecondsWarning) {
		t.Fatalf("warnings %q treat a different key as the retired one", out)
	}
}

// Empty/nil: an sre block that is not a mapping, or no sre block, loads
// without touching the retired-key path.
func TestRetiredCanaryKeyIgnoresOtherShapes(t *testing.T) {
	for _, body := range []string{"sre:\n", "sre:\n  autonomy:\n", "mode: standalone\n"} {
		if _, out, err := loadWithWarnings(t, body); err != nil ||
			strings.Contains(out, settleSecondsWarning) {
			t.Fatalf("%q: err = %v, warnings %q", body, err, out)
		}
	}
}
