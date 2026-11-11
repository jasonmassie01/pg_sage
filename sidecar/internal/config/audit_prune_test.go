package config

import "testing"

// No concurrent access tests here: loading a config is single-threaded.

// Pruning is off unless the operator turns it on: with no config file,
// and with an audit block that does not name the key.
func TestPruneBelowSignedCheckpointDefaultsOff(t *testing.T) {
	if DefaultConfig().Audit.PruneBelowSignedCheckpoint {
		t.Fatal("prune_below_signed_checkpoint defaults to true")
	}
	cfg := loadAuditYAML(t, "audit:\n  verify_interval_hours: 6\n")
	if cfg.Audit.PruneBelowSignedCheckpoint {
		t.Fatal("a partial audit block turned pruning on")
	}
	cfg = loadAuditYAML(t, "audit:\n  prune_below_signed_checkpoint: true\n")
	if !cfg.Audit.PruneBelowSignedCheckpoint || cfg.Audit.FullVerifyIntervalHours != 168 {
		t.Fatalf("explicit true: prune=%v full=%d", cfg.Audit.PruneBelowSignedCheckpoint,
			cfg.Audit.FullVerifyIntervalHours)
	}
}

// The key is documented and classed as safety-critical: it deletes audit
// links.
func TestPruneBelowSignedCheckpointIsDocumented(t *testing.T) {
	doc, ok := DescribeField(nil, "audit.prune_below_signed_checkpoint")
	if !ok || doc.Secret || doc.Doc == "" {
		t.Fatalf("describe audit.prune_below_signed_checkpoint = %+v, %v", doc, ok)
	}
	got, ok := KeyClassOf("audit.prune_below_signed_checkpoint")
	if !ok || got != KeySafetyCritical {
		t.Fatalf("key class = %q (%v), want safety_critical", got, ok)
	}
}
