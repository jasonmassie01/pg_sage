package config

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// ValidateDisabledRules checks a rule pack against the finding category
// registry (market-11): every name must be an analyzer rule or category,
// and the protected ones (wraparound, sequence exhaustion, the sage_* self
// rules) are refused with the reason.
func ValidateDisabledRules(field string, names []string) error {
	if _, err := findingcat.ResolveDisabled(names); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

// ParseRuleList splits the API form of a rule list ("a,b"): names are
// trimmed and empty entries dropped. The empty string is an empty list.
func ParseRuleList(value string) []string {
	out := []string{}
	for _, part := range strings.Split(value, ",") {
		if name := strings.TrimSpace(part); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// validateRulePacks validates the global and per-database disabled rules
// and the owner checks.
func (c *Config) validateRulePacks() error {
	if err := ValidateDisabledRules("analyzer.disabled_rules",
		c.Analyzer.DisabledRules); err != nil {
		return err
	}
	for i, db := range c.Databases {
		field := fmt.Sprintf("databases[%d].disabled_rules", i)
		if err := ValidateDisabledRules(field, db.DisabledRules); err != nil {
			return err
		}
	}
	_, err := PrepareChecks(c.Checks)
	return err
}
