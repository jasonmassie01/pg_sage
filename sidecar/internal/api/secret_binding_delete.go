package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/store"
)

// deleteMayRebind reports whether deleting key's override can change which
// origin a stored secret is sent to.
func deleteMayRebind(key string) bool {
	for _, b := range secretBindings {
		if deleteTouches(b, key) {
			return true
		}
	}
	return false
}

// boundDeleteRefusal returns a refusal message when deleting key's global
// override would send a stored secret to an origin it was not saved for.
// candidate is the config after the delete; nil builds it from the store.
// The error return is an internal failure, not a refusal.
func boundDeleteRefusal(
	ctx context.Context, cs *store.ConfigStore, base configBaseSource,
	key string, current, candidate *config.Config,
) (string, error) {
	if current == nil || !deleteMayRebind(key) {
		return "", nil
	}
	baseCfg, err := base()
	if err != nil {
		return "", fmt.Errorf("load config base: %w", err)
	}
	if candidate == nil {
		candidate, err = globalCandidateWithoutOverride(ctx, cs, base, key)
		if err != nil {
			return "", fmt.Errorf("build config without %s: %w", key, err)
		}
	}
	if err := deleteBindingError(key, current, candidate, baseCfg); err != nil {
		return err.Error(), nil
	}
	return "", nil
}

// deleteBindingError applies the binding rule to a delete. Deleting an
// override falls back to the config file, so the file's secret may come
// back next to an endpoint moved through the API. A secret from the file is
// bound to the file's endpoint origin. A secret set through the API was
// bound to the endpoint at the time, so moving the endpoint back to the
// file's is allowed, except when leaving llm.auth=databricks, under which
// the endpoint may have moved without the key.
func deleteBindingError(key string, current, candidate, base *config.Config) error {
	for _, b := range secretBindings {
		if !deleteTouches(b, key) {
			continue
		}
		secret := b.secret(candidate)
		if secret == "" || (b.databricksExempt && candidate.LLM.Auth == "databricks") {
			continue
		}
		endpoint, baseEndpoint := b.endpoint(candidate), b.endpoint(base)
		fileBound := secret == b.secret(base) &&
			(endpoint == baseEndpoint || egress.SameOrigin(endpoint, baseEndpoint))
		leaving := b.databricksExempt && current.LLM.Auth == "databricks"
		if fileBound || (secret != b.secret(base) && !leaving) {
			continue
		}
		if key == b.secretKey {
			return fmt.Errorf("deleting %s would send the config file's %s to %s on "+
				"another origin: delete %s first, or set %s", key, b.secretKey,
				b.endpointKey, b.endpointKey, b.secretKey)
		}
		return fmt.Errorf("deleting %s would send the stored %s to an origin it was "+
			"not saved for: set %s (or send it empty to clear it) first",
			key, b.secretKey, b.secretKey)
	}
	return nil
}

func deleteTouches(b secretBinding, key string) bool {
	return key == b.endpointKey || key == b.secretKey ||
		(b.databricksExempt && key == "llm.auth")
}

// refuseUnboundDelete writes the response and returns false when the delete
// must not proceed: 400 for a binding refusal, 500 for an internal failure.
// A nil candidate means current is the live config, read under the
// hot-reload lock.
func refuseUnboundDelete(
	w http.ResponseWriter, r *http.Request, cs *store.ConfigStore,
	base configBaseSource, key string, current, candidate *config.Config,
) bool {
	if candidate == nil && deleteMayRebind(key) && current != nil {
		config.RLockForHotReload()
		current = config.Clone(current)
		config.RUnlockForHotReload()
	}
	refusal, err := boundDeleteRefusal(r.Context(), cs, base, key, current, candidate)
	if err != nil {
		internalError(w, r, "check secret binding for delete", err)
		return false
	}
	if refusal != "" {
		jsonError(w, refusal, http.StatusBadRequest)
		return false
	}
	return true
}

// controlledDeleteCandidate builds the config without key's override and
// applies the binding rule to it; false means the response is written.
func controlledDeleteCandidate(
	w http.ResponseWriter, r *http.Request, cs *store.ConfigStore,
	base configBaseSource, key string, current *config.Config,
) (*config.Config, bool) {
	candidate, err := globalCandidateWithoutOverride(r.Context(), cs, base, key)
	if err != nil {
		internalError(w, r, "load desired config revision", err)
		return nil, false
	}
	return candidate, refuseUnboundDelete(w, r, cs, base, key, current, candidate)
}
