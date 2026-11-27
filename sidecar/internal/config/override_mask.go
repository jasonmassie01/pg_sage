package config

import "time"

// MaskedKey is an explicit file-layer value (YAML, env or flag) that a
// persisted UI override replaces (config-03). The values are display text:
// a secret key's values are redacted before they reach a MaskedKey.
type MaskedKey struct {
	Key            string    `json:"key"`
	FileValue      string    `json:"file_value"`
	OverrideValue  string    `json:"override_value"`
	EffectiveValue string    `json:"effective_value"`
	UpdatedBy      int       `json:"updated_by"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// overrideMask is what an override replay left on a config: the file layer
// it replayed onto, so a later PUT can re-mask, and the masked keys.
type overrideMask struct {
	base   *Config
	masked []MaskedKey
}

// SetOverrideMask records the file layer the overrides replayed onto and
// the keys they mask. Both are copied; base keeps its presence set.
func (c *Config) SetOverrideMask(base *Config, masked []MaskedKey) {
	if c == nil {
		return
	}
	var detached *Config
	if base != nil {
		detached = Clone(base)
		detached.overrideMask = nil
	}
	c.overrideMask = &overrideMask{
		base:   detached,
		masked: append([]MaskedKey(nil), masked...),
	}
}

// OverrideFileBase returns a copy of the file layer the overrides replayed
// onto, or nil when no replay recorded one.
func (c *Config) OverrideFileBase() *Config {
	if c == nil || c.overrideMask == nil || c.overrideMask.base == nil {
		return nil
	}
	return Clone(c.overrideMask.base)
}

// MaskedByOverride returns a copy of the masked keys, sorted by key; nil
// when nothing is masked.
func (c *Config) MaskedByOverride() []MaskedKey {
	if c == nil || c.overrideMask == nil || len(c.overrideMask.masked) == 0 {
		return nil
	}
	return append([]MaskedKey(nil), c.overrideMask.masked...)
}

// IsRestrictiveTrustKey reports the keys where an explicit file value and
// an override combine to the most restrictive instead of the override
// winning: the trust level and the tier3 switches.
func IsRestrictiveTrustKey(key string) bool {
	switch key {
	case "trust.level", "trust.tier3_safe", "trust.tier3_moderate",
		"trust.tier3_high_risk":
		return true
	}
	return false
}

// RestrictTrust lowers effective's value at key to file's when file's is
// more restrictive: the lower trust rank, or the AND of a tier3 switch.
// Other keys are left as they are.
func RestrictTrust(effective, file *Config, key string) {
	if effective == nil || file == nil {
		return
	}
	t, f := &effective.Trust, &file.Trust
	switch key {
	case "trust.level":
		if TrustRank(f.Level) < TrustRank(t.Level) {
			t.Level = f.Level
		}
	case "trust.tier3_safe":
		t.Tier3Safe = t.Tier3Safe && f.Tier3Safe
	case "trust.tier3_moderate":
		t.Tier3Moderate = t.Tier3Moderate && f.Tier3Moderate
	case "trust.tier3_high_risk":
		t.Tier3HighRisk = t.Tier3HighRisk && f.Tier3HighRisk
	}
}
