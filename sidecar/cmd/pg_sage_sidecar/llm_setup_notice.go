package main

import "github.com/pg-sage/sidecar/internal/config"

// noticeLLMSetup tells the operator, once at startup, that LLM features
// are on but no provider is configured, and how to configure or disable
// them. Feature code then falls back silently, so this is the only line.
func noticeLLMSetup(c *config.Config, logf func(component, msg string, args ...any)) {
	if note := c.LLMSetupNotice(); note != "" {
		logf("llm", "%s", note)
	}
}
