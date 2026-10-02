package config

// LLMSetupNotice returns the one-line startup notice for an LLM that is
// enabled (the default) but cannot reach a provider, or "" when there is
// nothing to say. The provider check mirrors llm.Client enablement:
// enabled with an endpoint and an API key. Without one every LLM-backed
// feature runs its deterministic path.
func (c *Config) LLMSetupNotice() string {
	if c == nil || !c.LLM.Enabled ||
		(c.LLM.Endpoint != "" && c.LLM.APIKey != "") {
		return ""
	}
	return "LLM features are on by default but no LLM is configured: set " +
		"llm.endpoint and llm.api_key (or SAGE_LLM_ENDPOINT and " +
		"SAGE_LLM_API_KEY) to use them; until then pg_sage runs " +
		"deterministic analysis only. Set llm.enabled: false to turn LLM " +
		"features off and silence this notice."
}
