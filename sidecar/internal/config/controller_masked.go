package config

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// maskedNotices remembers which masked overrides were logged, so each one
// is logged once per distinct (file value, override value) pair rather than
// on every reload (config-03).
type maskedNotices struct {
	mu   sync.Mutex
	log  func(string)
	seen map[string]struct{}
}

// SetMaskedOverrideLogger routes the masked-override notices, then logs the
// masks of the current desired revision. nil restores the default (slog).
func (c *ConfigController) SetMaskedOverrideLogger(log func(string)) {
	c.masked.mu.Lock()
	c.masked.log = log
	c.masked.mu.Unlock()
	c.noteMasked(c.Desired().Config)
}

// MaskedByOverride lists the explicit file values the desired revision's
// persisted overrides mask; nil when none do.
func (c *ConfigController) MaskedByOverride() []MaskedKey {
	return c.Desired().Config.MaskedByOverride()
}

func (c *ConfigController) noteMasked(cfg *Config) {
	masked := cfg.MaskedByOverride()
	if len(masked) == 0 {
		return
	}
	c.masked.mu.Lock()
	defer c.masked.mu.Unlock()
	if c.masked.seen == nil {
		c.masked.seen = make(map[string]struct{})
	}
	log := c.masked.log
	if log == nil {
		log = func(msg string) { slog.Warn(msg) }
	}
	for _, m := range masked {
		pair := m.Key + "\x00" + m.FileValue + "\x00" + m.OverrideValue
		if _, done := c.masked.seen[pair]; done {
			continue
		}
		c.masked.seen[pair] = struct{}{}
		log(maskedNotice(m))
	}
}

// maskedNotice names the key, both values, who stored the override and
// when, and which value applies.
func maskedNotice(m MaskedKey) string {
	rule := "the override wins; Reset in Settings (DELETE " +
		"/api/v1/config/global/" + m.Key + ") removes it"
	if IsRestrictiveTrustKey(m.Key) {
		rule = "trust settings take the more restrictive value"
	}
	at := "an unknown time"
	if !m.UpdatedAt.IsZero() {
		at = m.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("config: a stored UI override masks %s: the config file, "+
		"environment or a flag sets %q, the override (user %d, %s) sets %q; "+
		"effective %q (%s)", m.Key, m.FileValue, m.UpdatedBy, at, m.OverrideValue,
		m.EffectiveValue, rule)
}
