package fleet

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/executor"
	prov "github.com/pg-sage/sidecar/internal/provider"
)

type ProviderAdapter struct {
	Provider         string
	Extensions       map[string]string
	LogAccess        string
	Limitations      []string
	SupportedActions map[string]bool
}

// AdapterForProvider is the readiness adapter for provider: the registry's
// canonical name and log access, the provider's extension readiness and
// limitations, and the action families its executor contracts support
// minus the profile's exclusions. An unknown provider supports nothing.
func AdapterForProvider(provider string) ProviderAdapter {
	profile, ok := prov.Lookup(provider)
	if !ok {
		adapter := managedAdapter(normalizeProviderName(provider),
			defaultExtensionReadiness(), []string{"provider adapter unknown"})
		adapter.SupportedActions = map[string]bool{}
		return adapter
	}
	spec := adapterReadiness[profile.Name]
	extensions := spec.extensions
	if extensions == nil {
		extensions = defaultExtensionReadiness()
	}
	adapter := managedAdapter(profile.Name, extensions, spec.limitations)
	adapter.SupportedActions = supportedFamilies(profile)
	return adapter
}

// supportedFamilies are the contracts that list the provider (or list no
// provider): exactly the families the standing gate does not block as
// provider_unsupported.
func supportedFamilies(profile prov.Profile) map[string]bool {
	out := map[string]bool{}
	for _, actionType := range executor.ContractActionTypes() {
		contract, ok := executor.ContractForActionType(actionType)
		if !ok {
			continue
		}
		if contractAllowsProvider(contract.ProviderSupport, profile.Name) {
			out[actionType] = true
		}
	}
	return out
}

// contractAllowsProvider mirrors the policy gate: an empty support list
// allows every provider.
func contractAllowsProvider(support []string, name string) bool {
	if len(support) == 0 {
		return true
	}
	for _, item := range support {
		if strings.EqualFold(item, name) {
			return true
		}
	}
	return false
}

// managedAdapter builds an adapter whose log access comes from the
// provider registry ("unknown" for a provider it does not know).
func managedAdapter(
	provider string,
	extensions map[string]string,
	limitations []string,
) ProviderAdapter {
	logAccess := "unknown"
	if profile, ok := prov.Lookup(provider); ok {
		logAccess = profile.LogAccess
	}
	return ProviderAdapter{
		Provider:    provider,
		Extensions:  copyExtensions(extensions),
		LogAccess:   logAccess,
		Limitations: append([]string(nil), limitations...),
	}
}

func (a ProviderAdapter) SupportsAction(actionType string) bool {
	if len(a.SupportedActions) == 0 {
		return false
	}
	return a.SupportedActions[actionType]
}

func copyExtensions(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
