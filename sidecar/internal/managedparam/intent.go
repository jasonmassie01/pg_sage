// Package managedparam turns a setting change that SQL cannot make on a
// managed PostgreSQL service (a restart-required or provider-restricted
// parameter on RDS, Aurora, Cloud SQL, AlloyDB, Azure flexible server or a
// CloudNativePG Cluster) into a typed proposal: the exact parameter-group,
// database-flag, server-parameter or Cluster-spec change, whether it needs
// a reboot, its rollback and the CLI command an operator runs. pg_sage never applies these changes
// itself; it records the operator's decision, observes when the new value
// is running and reports parameter drift.
package managedparam

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	prov "github.com/pg-sage/sidecar/internal/provider"
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
	// providerValuePattern bounds a value read from a provider API (a
	// current Cloud SQL flag or RDS parameter) that a command or rollback
	// carries: printable ASCII without whitespace, quotes, backslash or the
	// shell's expansion and control characters, at most 256 characters.
	// Commas and the gcloud alternate delimiters (@ : | ~) are allowed
	// because the builders escape them.
	providerValuePattern = regexp.MustCompile(`^[A-Za-z0-9._+,/:@|~%{}*-]{0,256}$`)
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

// retiredProvider is an alias of a supported profile whose service this
// package cannot describe: Azure single server is retired and has no
// flexible-server parameter commands.
const retiredProvider = "azure-single"

// NormalizeProvider maps the cloud environment names pg_sage detects to
// the providers this package supports ("" when unsupported): those whose
// settings change through a parameter group, Cloud SQL or AlloyDB database
// flags, Azure flexible-server parameters or a CloudNativePG Cluster spec.
func NormalizeProvider(provider string) string {
	if strings.EqualFold(strings.TrimSpace(provider), retiredProvider) {
		return ""
	}
	profile, _ := prov.Lookup(provider)
	switch profile.ConfigMechanism {
	case prov.ConfigParameterGroup, prov.ConfigClusterParameterGroup,
		prov.ConfigCloudSQLFlags, prov.ConfigAlloyDBFlags, prov.ConfigServerParameters,
		prov.ConfigClusterSpec:
		return profile.Name
	}
	return ""
}

// Supported reports whether provider changes settings through a parameter
// group, database flags or server parameters this package can describe.
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
