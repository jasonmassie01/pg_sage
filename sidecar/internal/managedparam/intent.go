// Package managedparam turns a setting change that SQL cannot make on a
// managed PostgreSQL service (a restart-required or provider-restricted
// parameter on RDS, Aurora or Cloud SQL) into a typed proposal: the exact
// parameter-group or database-flag change, whether it needs a reboot, its
// rollback and the CLI command an operator runs. pg_sage never applies
// these changes itself; it records the operator's decision, observes when
// the new value is running and reports parameter drift.
package managedparam

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DetailKey is the finding Detail key that carries an Intent.
const DetailKey = "managed_change"

var (
	ErrUnsupportedProvider = errors.New("managed parameter changes are not supported for " +
		"this provider")
	ErrInvalidParameter = errors.New("invalid parameter name")
	ErrInvalidValue     = errors.New("invalid parameter value")
	ErrInvalidTarget    = errors.New("invalid parameter group or instance name")
	ErrNotModifiable    = errors.New("the provider does not allow changing this parameter")
)

var (
	parameterPattern = regexp.MustCompile(`^[a-z][a-z0-9_.]{0,62}$`)
	// intentValuePattern bounds what may reach a CLI command line: no
	// spaces, quotes, separators or shell metacharacters.
	intentValuePattern = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,32}$`)
)

// Intent is what a producer (the configuration advisor or the tuning
// agent) wants changed, in PostgreSQL's form ("4GB"), and why SQL cannot
// do it.
type Intent struct {
	Provider  string `json:"provider"`
	Parameter string `json:"parameter"`
	Value     string `json:"value"`
	Reason    string `json:"reason"`
}

// NormalizeProvider maps the cloud environment names pg_sage detects to
// the providers this package supports ("" when unsupported).
func NormalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "rds", "aws":
		return "rds"
	case "aurora":
		return "aurora"
	case "cloud-sql", "cloudsql", "gcp":
		return "cloud-sql"
	}
	return ""
}

// Supported reports whether provider changes settings through a parameter
// group or database flags this package can describe.
func Supported(provider string) bool { return NormalizeProvider(provider) != "" }

// NewIntent validates an intent.
func NewIntent(provider, parameter, value, reason string) (Intent, error) {
	in := Intent{Provider: NormalizeProvider(provider),
		Parameter: strings.ToLower(strings.TrimSpace(parameter)),
		Value:     strings.TrimSpace(value), Reason: strings.TrimSpace(reason)}
	return in, in.validate()
}

func (i Intent) validate() error {
	switch {
	case NormalizeProvider(i.Provider) == "":
		return fmt.Errorf("%w: %q", ErrUnsupportedProvider, i.Provider)
	case !parameterPattern.MatchString(i.Parameter):
		return fmt.Errorf("%w: %q", ErrInvalidParameter, i.Parameter)
	case !intentValuePattern.MatchString(i.Value):
		return fmt.Errorf("%w: %q", ErrInvalidValue, i.Value)
	}
	return nil
}

// Detail is the intent in the form stored in a finding's detail.
func (i Intent) Detail() map[string]any {
	return map[string]any{"provider": i.Provider, "parameter": i.Parameter,
		"value": i.Value, "reason": i.Reason}
}

// IntentFromDetail reads and validates the intent in a finding's detail.
func IntentFromDetail(detail map[string]any) (Intent, bool) {
	raw, ok := detail[DetailKey].(map[string]any)
	if !ok {
		return Intent{}, false
	}
	str := func(key string) (string, bool) {
		v, ok := raw[key].(string)
		return v, ok
	}
	provider, ok1 := str("provider")
	parameter, ok2 := str("parameter")
	value, ok3 := str("value")
	reason, _ := str("reason")
	if !ok1 || !ok2 || !ok3 {
		return Intent{}, false
	}
	in, err := NewIntent(provider, parameter, value, reason)
	if err != nil {
		return Intent{}, false
	}
	return in, true
}
