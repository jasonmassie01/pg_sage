package api

import (
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// config-03: a persisted override still wins over the file layer, except
// for the trust ceilings, but it no longer does so silently. Each override
// that replaces an explicit file value (YAML, env or flag) is masked: it is
// logged, listed by GET /config/global and badged in Settings.

// maskedOverride checks the override just applied to candidate at key. When
// it replaced an explicit, different file value, a trust ceiling falls back
// to the more restrictive of the two and the key is reported as masked.
func maskedOverride(
	candidate, base *config.Config, key string, by int, at time.Time,
) (config.MaskedKey, bool) {
	if !base.IsExplicit(key) {
		return config.MaskedKey{}, false
	}
	fileValue, okFile := config.ValueAtPath(base, key)
	overrideValue, okOverride := config.ValueAtPath(candidate, key)
	if !okFile || !okOverride || reflect.DeepEqual(fileValue, overrideValue) {
		return config.MaskedKey{}, false
	}
	m := config.MaskedKey{Key: key, UpdatedBy: by, UpdatedAt: at}
	if config.IsRestrictiveTrustKey(key) {
		// Never a secret, so the raw value is the display text.
		m.OverrideValue = fmt.Sprint(overrideValue)
		config.RestrictTrust(candidate, base, key)
	}
	return m, true
}

// recordMasked fills the display values from the redacted read models,
// sorts by key and records the list and the file base on candidate.
func recordMasked(
	candidate, base *config.Config, masked []config.MaskedKey,
) []config.MaskedKey {
	if len(masked) > 0 {
		fileModel := store.ConfigReadModel(base)
		effectiveModel := store.ConfigReadModel(candidate)
		for i := range masked {
			m := &masked[i]
			m.FileValue = readModelText(fileModel, base, m.Key)
			m.EffectiveValue = readModelText(effectiveModel, candidate, m.Key)
			if m.OverrideValue == "" {
				m.OverrideValue = m.EffectiveValue
			}
		}
		sort.Slice(masked, func(i, j int) bool { return masked[i].Key < masked[j].Key })
	}
	candidate.SetOverrideMask(base, masked)
	return candidate.MaskedByOverride()
}

// readModelText is the value as the config API shows it, secrets masked. A
// key the read model omits is never a secret (they are all listed there).
func readModelText(model map[string]any, cfg *config.Config, key string) string {
	if entry, ok := model[key].(map[string]any); ok {
		return fmt.Sprint(entry["value"])
	}
	value, _ := config.ValueAtPath(cfg, key)
	return fmt.Sprint(value)
}

// applyMaskedWrites re-masks the keys a PUT just wrote onto candidate (the
// desired config, already carrying the replayed mask). A config without a
// recorded file layer keeps overrides winning everywhere.
func applyMaskedWrites(
	candidate *config.Config, writes []store.ConfigOverrideWrite,
	by int, at time.Time,
) {
	base := candidate.OverrideFileBase()
	if base == nil {
		return
	}
	written := make(map[string]bool, len(writes))
	for _, write := range writes {
		written[write.Key] = true
	}
	var masked []config.MaskedKey
	for _, m := range candidate.MaskedByOverride() {
		if !written[m.Key] {
			masked = append(masked, m)
		}
	}
	for _, write := range writes {
		if m, isMasked := maskedOverride(candidate, base, write.Key, by, at); isMasked {
			masked = append(masked, m)
		}
	}
	recordMasked(candidate, base, masked)
}

// markMaskedEntries flags each masked key in the merged read model and
// shows its effective value: for a held-back trust raise that is the file
// value, not the stored override.
func markMaskedEntries(merged map[string]any, desired *config.Config) {
	masked := desired.MaskedByOverride()
	if len(masked) == 0 {
		return
	}
	effective := store.ConfigReadModel(desired)
	for _, m := range masked {
		entry, ok := merged[m.Key].(map[string]any)
		if !ok {
			continue
		}
		entry["masked"] = true
		if value, ok := effective[m.Key].(map[string]any); ok {
			entry["value"] = value["value"]
		}
	}
}

// maskedResponse is the masked_by_override array: never null.
func maskedResponse(desired *config.Config) []config.MaskedKey {
	masked := desired.MaskedByOverride()
	if masked == nil {
		return []config.MaskedKey{}
	}
	return masked
}
