package api

import (
	"fmt"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/egress"
	"github.com/pg-sage/sidecar/internal/store"
)

// secretBinding ties a stored secret to the endpoint it was saved for. The
// API never returns these secrets, so an endpoint edit that kept the
// stored secret would hand it to whatever host the new endpoint names.
type secretBinding struct {
	endpointKey string
	secretKey   string
	endpoint    func(*config.Config) string
	secret      func(*config.Config) string
	// databricksExempt skips the check under llm.auth=databricks, where the
	// workspace origin is already pinned and no API key is sent.
	databricksExempt bool
}

var secretBindings = []secretBinding{
	{
		endpointKey: "llm.endpoint", secretKey: "llm.api_key",
		endpoint:         func(c *config.Config) string { return c.LLM.Endpoint },
		secret:           func(c *config.Config) string { return c.LLM.APIKey },
		databricksExempt: true,
	},
	{
		endpointKey: "clone.dle_endpoint", secretKey: "clone.dle_token",
		endpoint: func(c *config.Config) string { return c.Clone.DLEEndpoint },
		secret:   func(c *config.Config) string { return c.Clone.DLEToken },
	},
}

// boundConfigWrites is validatedConfigWrites for a global write: it first
// refuses a write that would send a stored secret to a new origin.
func boundConfigWrites(
	body map[string]any, current *config.Config,
) ([]store.ConfigOverrideWrite, []string) {
	if err := bindSecretDestinations(body, current); err != nil {
		return nil, []string{err.Error()}
	}
	return validatedConfigWrites(body, configTargetPolicy(current))
}

// bindSecretDestinations refuses a config write that moves an endpoint to a
// new origin (scheme, host or effective port) while a secret is stored for
// it, or that switches llm.auth away from databricks while llm.api_key is
// stored, unless the same write supplies that secret unmasked. An empty secret
// clears the stored one in the same write. It reads body and current only.
func bindSecretDestinations(body map[string]any, current *config.Config) error {
	if current == nil {
		return nil
	}
	for _, binding := range secretBindings {
		if err := binding.check(body, current); err != nil {
			return err
		}
	}
	return nil
}

func (b secretBinding) check(body map[string]any, current *config.Config) error {
	reason := b.moveReason(body, current)
	if reason == "" || b.secret(current) == "" {
		return nil
	}
	if secret, ok := body[b.secretKey]; ok &&
		!isMaskedSecretUpdate(b.secretKey, fmt.Sprintf("%v", secret)) {
		return nil
	}
	return fmt.Errorf("%s: include %s, or send it empty to clear it", reason, b.secretKey)
}

// moveReason says why the write sends the stored secret somewhere it was
// not saved for, or "" when it does not. Under the databricks exemption an
// endpoint may move without the key, so leaving databricks auth later must
// ask for the key too: otherwise it would reach the endpoint moved earlier.
func (b secretBinding) moveReason(body map[string]any, current *config.Config) string {
	if b.databricksExempt && effectiveLLMAuth(body, current) == "databricks" {
		return ""
	}
	if raw, ok := body[b.endpointKey]; ok {
		next, stored := fmt.Sprintf("%v", raw), b.endpoint(current)
		if next != stored && !egress.SameOrigin(next, stored) {
			return b.endpointKey + " moves to a new origin"
		}
	}
	if b.databricksExempt && current.LLM.Auth == "databricks" {
		return "llm.auth leaves databricks"
	}
	return ""
}

// effectiveLLMAuth is llm.auth after the write: the body's value if it sets
// one, else the current one.
func effectiveLLMAuth(body map[string]any, current *config.Config) string {
	if raw, ok := body["llm.auth"]; ok {
		return fmt.Sprintf("%v", raw)
	}
	return current.LLM.Auth
}
