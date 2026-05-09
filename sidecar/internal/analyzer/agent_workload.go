package analyzer

import (
	"fmt"
	"strings"
)

const (
	AgentWorkloadAttributed   = "attributed"
	AgentWorkloadUnattributed = "unattributed"
	AgentWorkloadEphemeral    = "ephemeral"
)

type AgentWorkloadSignal struct {
	ApplicationName string
	QueryText       string
	DatabaseName    string
}

type AgentWorkloadClassification struct {
	Kind     string
	AgentID  string
	Evidence []string
}

func ClassifyAgentWorkload(signal AgentWorkloadSignal) AgentWorkloadClassification {
	app := strings.ToLower(strings.TrimSpace(signal.ApplicationName))
	query := strings.ToLower(signal.QueryText)
	if app == "" && !strings.Contains(query, "agent") {
		return AgentWorkloadClassification{
			Kind:     AgentWorkloadUnattributed,
			Evidence: []string{"empty application_name"},
		}
	}
	if strings.Contains(app, "tmp") || strings.Contains(app, "sandbox") ||
		strings.Contains(query, "create temporary table") {
		return AgentWorkloadClassification{
			Kind:     AgentWorkloadEphemeral,
			AgentID:  signal.ApplicationName,
			Evidence: []string{"ephemeral workspace signal"},
		}
	}
	if id := extractAgentID(app, query); id != "" {
		return AgentWorkloadClassification{
			Kind:     AgentWorkloadAttributed,
			AgentID:  id,
			Evidence: []string{"agent attribution present"},
		}
	}
	return AgentWorkloadClassification{
		Kind:     AgentWorkloadUnattributed,
		Evidence: []string{"no stable agent identifier"},
	}
}

func AgentWorkloadFinding(signal AgentWorkloadSignal) *Finding {
	classification := ClassifyAgentWorkload(signal)
	if classification.Kind == AgentWorkloadAttributed {
		return nil
	}
	identifier := signal.DatabaseName
	if identifier == "" {
		identifier = "current_database"
	}
	title := "Agent workload lacks stable attribution"
	recommendation := "Set application_name or SQL comments with a stable agent identifier."
	if classification.Kind == AgentWorkloadEphemeral {
		title = "Agent workload appears ephemeral"
		recommendation = "Tag ephemeral agent databases so pg_sage can separate experiments from durable traffic."
	}
	return &Finding{
		Category:         "agent_workload",
		Severity:         "info",
		ObjectType:       "database",
		ObjectIdentifier: identifier,
		Title:            title,
		Detail: map[string]any{
			"kind":             classification.Kind,
			"application_name": signal.ApplicationName,
			"evidence":         classification.Evidence,
		},
		Recommendation: recommendation,
	}
}

func extractAgentID(appName string, query string) string {
	for _, source := range []string{appName, query} {
		if id := taggedValue(source, "agent:"); id != "" {
			return id
		}
		if id := taggedValue(source, "agent_id="); id != "" {
			return id
		}
	}
	return ""
}

func taggedValue(source string, prefix string) string {
	idx := strings.Index(source, prefix)
	if idx < 0 {
		return ""
	}
	rest := source[idx+len(prefix):]
	end := len(rest)
	for i, r := range rest {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			end = i
			break
		}
	}
	id := strings.Trim(rest[:end], " -_./*")
	if id == "" {
		return ""
	}
	return fmt.Sprintf("agent:%s", id)
}
