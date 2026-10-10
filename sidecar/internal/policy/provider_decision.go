package policy

import "strings"

// providerDecision blocks actions whose contract excludes the target's
// provider (formerly checked only by the legacy executor engine).
func providerDecision(runtime RuntimeState, req ActionRequest) (Decision, bool) {
	support := req.Contract.ProviderSupport
	if len(support) == 0 {
		return Decision{}, false
	}
	provider := strings.ToLower(strings.TrimSpace(runtime.Provider))
	if provider == "" || provider == "self-managed" {
		provider = "postgres"
	}
	for _, item := range support {
		if strings.EqualFold(provider, item) {
			return Decision{}, false
		}
	}
	return blocked(ReasonProviderUnsupported, "provider "+provider), true
}
