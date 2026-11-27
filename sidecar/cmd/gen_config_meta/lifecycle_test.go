package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

const committedMetaPath = "../../web/src/generated/config_meta.json"

// namedOwner is a registered owner that is never asked to prepare: the
// drift test only asks the controller which lifecycle a key has.
type namedOwner string

func (o namedOwner) Name() string { return string(o) }

func (o namedOwner) Prepare(context.Context, config.ConfigSnapshot,
	config.ConfigSnapshot) (config.PreparedReconfiguration, error) {
	return nil, nil
}

// standaloneController registers the owners every mode registers, as a
// standalone sidecar does (no YAML fleet databases owner).
func standaloneController() *config.ConfigController {
	var owners []config.ReconfigurationOwner
	for _, name := range everyModeOwners() {
		owners = append(owners, namedOwner(name))
	}
	return config.NewConfigController(config.DefaultConfig(), nil, owners...)
}

func readCommittedMeta(t *testing.T) map[string]fieldMeta {
	t.Helper()
	raw, err := os.ReadFile(committedMetaPath)
	if err != nil {
		t.Fatalf("read %s: %v", committedMetaPath, err)
	}
	var meta map[string]fieldMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("decode %s: %v", committedMetaPath, err)
	}
	if len(meta) < 100 {
		t.Fatalf("committed meta has %d keys, want the whole config", len(meta))
	}
	return meta
}

var validLifecycles = map[string]bool{
	string(config.LifecycleLivePolicy):  true,
	string(config.LifecycleReconfigure): true,
	string(config.LifecycleRestart):     true,
}

// Drift: every committed key carries a lifecycle the generator would
// write today.
func TestLifecycle_EveryCommittedKeyHasCurrentLifecycle(t *testing.T) {
	committed := readCommittedMeta(t)
	generated, err := Generate(config.DefaultConfig(), false)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	for key, entry := range committed {
		if !validLifecycles[entry.Lifecycle] {
			t.Errorf("%s lifecycle = %q, want live_policy, reconfigure or restart",
				key, entry.Lifecycle)
			continue
		}
		if fresh := generated[key].Lifecycle; fresh != entry.Lifecycle {
			t.Errorf("%s committed lifecycle %q, generator says %q; "+
				"regenerate with `go run ./cmd/gen_config_meta/`", key, entry.Lifecycle, fresh)
		}
	}
}

// Drift: a top-level key's lifecycle is what a standalone controller
// applies, and a per-database key's is DatabaseFieldLifecycles'.
func TestLifecycle_MatchesStandaloneEffectiveLifecycle(t *testing.T) {
	controller := standaloneController()
	checked := 0
	for key, entry := range readCommittedMeta(t) {
		if field, ok := strings.CutPrefix(key, "databases[]."); ok {
			want, found := config.LookupDatabaseFieldLifecycle(registryPath(field))
			if !found || string(want.Lifecycle) != entry.Lifecycle {
				t.Errorf("%s lifecycle = %q, database registry %q (found %v)",
					key, entry.Lifecycle, want.Lifecycle, found)
			}
			continue
		}
		want, found := controller.EffectiveLifecycle(registryPath(key))
		if !found || string(want) != entry.Lifecycle {
			t.Errorf("%s lifecycle = %q, standalone controller %q (found %v)",
				key, entry.Lifecycle, want, found)
		}
		checked++
	}
	if checked < 100 {
		t.Fatalf("checked %d top-level keys, want the whole config", checked)
	}
}

func TestLifecycle_KnownKeys(t *testing.T) {
	for key, want := range map[string]config.ConfigLifecycle{
		"trust.level":                    config.LifecycleLivePolicy,
		"trust.maintenance_window":       config.LifecycleLivePolicy,
		"llm.api_key":                    config.LifecycleReconfigure,
		"collector.interval_seconds":     config.LifecycleRestart,
		"postgres.host":                  config.LifecycleRestart,
		"defaults.trust_level":           config.LifecycleRestart,
		"databases[].trust_level":        config.LifecycleLivePolicy,
		"databases[].host":               config.LifecycleReconfigure,
		"databases[].replicas[].dsn_env": config.LifecycleReconfigure,
	} {
		got, err := lifecycleFor(key)
		if err != nil || got != string(want) {
			t.Errorf("lifecycleFor(%q) = %q, %v; want %q", key, got, err, want)
		}
	}
}

// Invalid input: a key the registries do not know fails generation
// instead of shipping a field without a lifecycle.
func TestLifecycle_UnknownKeyFails(t *testing.T) {
	for _, key := range []string{"", "nope.key", "databases[].nope", "databases[]."} {
		if got, err := lifecycleFor(key); err == nil {
			t.Errorf("lifecycleFor(%q) = %q, want an error", key, got)
		} else if !strings.Contains(err.Error(), "lifecycle") {
			t.Errorf("lifecycleFor(%q) error %q does not name the lifecycle", key, err)
		}
	}
}

// The standalone owner set is every production owner but the YAML fleet's.
func TestLifecycle_EveryModeOwners(t *testing.T) {
	got := strings.Join(everyModeOwners(), ",")
	if got != "trust_policy,llm,llm_redaction,briefing,alerting" {
		t.Fatalf("everyModeOwners = %s, want trust_policy,llm,llm_redaction,briefing,alerting",
			got)
	}
}
