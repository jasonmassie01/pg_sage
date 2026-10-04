package onboarding

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/firstlook"
)

// Checklist step IDs, in order.
const (
	StepConnected     = "connected"
	StepExtensions    = "extensions"
	StepFirstLook     = "first_look"
	StepMCPToken      = "mcp_token"
	StepNotifications = "notifications"
	StepGrantMore     = "grant_more"
)

// Step is one first-run checklist entry. Link is an in-app route.
type Step struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Done     bool   `json:"done"`
	Optional bool   `json:"optional,omitempty"`
	Detail   string `json:"detail"`
	Link     string `json:"link,omitempty"`
}

// ChecklistInput is the live state the checklist reports. Nil pointers
// are unknown (e.g. the viewer may not see MCP tokens).
type ChecklistInput struct {
	Connected      bool
	Capabilities   []firstlook.Capability
	FirstLookReady bool
	FirstLookItems int
	MCPEnabled     bool
	MCPToken       *bool
	Notifications  *bool
	TrustLevel     string
	TTFFSeconds    *float64
}

// Checklist is the first-run checklist for one database.
func Checklist(in ChecklistInput) []Step {
	return []Step{connectedStep(in), extensionsStep(in), firstLookStep(in), mcpStep(in),
		notificationsStep(in), grantStep(in)}
}

func connectedStep(in ChecklistInput) Step {
	s := Step{ID: StepConnected, Label: "Connected", Done: in.Connected,
		Detail: "pg_sage is connected and monitoring."}
	if !in.Connected {
		s.Detail = "pg_sage cannot reach the database; check the connection URL and " +
			"network."
	}
	return s
}

func extensionsStep(in ChecklistInput) Step {
	s := Step{ID: StepExtensions, Label: "Extensions",
		Detail: "Checking extensions with the first look."}
	if len(in.Capabilities) == 0 {
		return s
	}
	var missing []string
	for _, c := range in.Capabilities {
		if c.Status == firstlook.CapabilityOK {
			if c.Name == firstlook.CapStatStatements {
				s.Done = true
			}
			continue
		}
		missing = append(missing, fmt.Sprintf("%s is %s", c.Name,
			strings.ReplaceAll(c.Status, "_", " ")))
	}
	switch {
	case !s.Done:
		s.Detail = strings.Join(missing, "; ") + ". The first look lists the exact " +
			"steps; until then query analysis is off."
	case len(missing) > 0:
		s.Detail = "pg_stat_statements is ready. Optional: " + strings.Join(missing, "; ") +
			"."
	default:
		s.Detail = "pg_stat_statements, hypopg and auto_explain are ready."
	}
	return s
}

func firstLookStep(in ChecklistInput) Step {
	s := Step{ID: StepFirstLook, Label: "First look ready", Done: in.FirstLookReady,
		Detail: "Reading the catalog (no query history needed)."}
	if !in.FirstLookReady {
		return s
	}
	if in.FirstLookItems == 0 {
		s.Detail = "No problems found in the catalog. Query findings follow as " +
			"statistics accumulate."
	} else {
		s.Detail = fmt.Sprintf("%d findings from the catalog.", in.FirstLookItems)
	}
	if in.TTFFSeconds != nil {
		s.Detail += fmt.Sprintf(" First finding %s after start.", seconds(*in.TTFFSeconds))
	}
	return s
}

func seconds(s float64) string {
	if s < 100 {
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.1f", s), "0"), ".") +
			" s"
	}
	return fmt.Sprintf("%.0f s", s)
}

func mcpStep(in ChecklistInput) Step {
	s := Step{ID: StepMCPToken, Label: "MCP token", Optional: true, Link: "#/mcp-tokens",
		Detail: "Create a token so coding agents (Claude Code, Cursor) can ask pg_sage."}
	switch {
	case !in.MCPEnabled:
		s.Detail = "MCP over HTTP is off (mcp.enabled, mcp.transport: http)."
	case in.MCPToken == nil:
		s.Detail = "Only admins can see whether an MCP token exists."
	case *in.MCPToken:
		s.Done, s.Detail = true, "An active MCP token exists."
	}
	return s
}

func notificationsStep(in ChecklistInput) Step {
	s := Step{ID: StepNotifications, Label: "Notifications", Optional: true,
		Link:   "#/notifications",
		Detail: "Add a channel (Slack, email, webhook) for findings and approval cards."}
	switch {
	case in.Notifications == nil:
		s.Detail = "Only admins can see the notification channels."
	case *in.Notifications:
		s.Done, s.Detail = true, "A notification channel is configured."
	}
	return s
}

func grantStep(in ChecklistInput) Step {
	s := Step{ID: StepGrantMore, Label: "Grant more", Link: "#/settings"}
	switch in.TrustLevel {
	case LevelAdvisory, LevelAutonomous:
		s.Done = true
		s.Detail = fmt.Sprintf("Trust level %s: pg_sage acts within it and verifies "+
			"every action.", in.TrustLevel)
	case LevelObservation:
		s.Detail = "Read-only: pg_sage observes and records what it would do; nothing " +
			"changes outside its sage schema until you grant more."
	default:
		s.Detail = "Trust level unknown."
	}
	return s
}
