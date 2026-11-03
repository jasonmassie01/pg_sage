package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// DatabricksAppMode is a process boundary, not a user-editable config key.
func DatabricksAppMode() bool {
	return os.Getenv("SAGE_AUTH_MODE") == "databricks_app" || os.Getenv("DATABRICKS_APP_PORT") != ""
}

// ValidateDatabricksApp rejects routing/credential downgrades in snapshots,
// including snapshots assembled from persisted overrides and derived clients.
func (l LLMConfig) ValidateDatabricksApp() error {
	if !DatabricksAppMode() {
		return nil
	}
	if l.APIKey != "" || l.OptimizerLLM.APIKey != "" {
		return fmt.Errorf("app mode forbids llm.api_key and llm.optimizer_llm.api_key; use platform OAuth")
	}
	if l.Auth != "" && l.Auth != "databricks" {
		return fmt.Errorf("app mode requires llm.auth=databricks")
	}
	if (l.Endpoint != "" || l.OptimizerLLM.Endpoint != "") && l.Auth != "databricks" {
		return fmt.Errorf("app mode requires llm.auth=databricks for configured AI clients")
	}
	for name, endpoint := range map[string]string{"llm.endpoint": l.Endpoint, "llm.optimizer_llm.endpoint": l.OptimizerLLM.Endpoint} {
		if endpoint == "" {
			continue
		}
		workspace := strings.TrimRight(os.Getenv("DATABRICKS_HOST"), "/")
		if !strings.Contains(workspace, "://") {
			workspace = "https://" + workspace
		}
		origin, oe := url.Parse(workspace)
		target, te := url.Parse(endpoint)
		if oe != nil || te != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || target.Scheme != "https" || target.User != nil || !strings.EqualFold(target.Host, origin.Host) || target.RawQuery != "" || target.Fragment != "" {
			return fmt.Errorf("app mode %s must use the configured HTTPS workspace origin without credentials, query or fragment", name)
		}
	}
	return nil
}
