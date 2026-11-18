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
	// FirstLookCounts sorts the first look's items: findings, setup notes,
	// posture notes and other notes.
	FirstLookCounts firstlook.ItemCounts
	// FirstLookChecks, FirstLookDegraded and FirstLookTruncated count the
	// catalog checks: all of them, those that could not run, and those that
	// found more than the report shows.
	FirstLookChecks    int
	FirstLookDegraded  int
	FirstLookTruncated int
	MCPEnabled         bool
	MCPToken           *bool
	Notifications      *bool
	TrustLevel         string
	TTFFSeconds        *float64
	// Model is the model step's status (nil: unknown); AutonomyChosen is
	// true once the setup wizard recorded an autonomy choice (ux-04b).
	Model          *ModelStatus
	AutonomyChosen bool
}

// Checklist is the first-run checklist for one database.
func Checklist(in ChecklistInput) []Step {
	return []Step{connectedStep(in), extensionsStep(in), firstLookStep(in), modelStep(in),
		mcpStep(in), notificationsStep(in), grantStep(in)}
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
		if c.Name == firstlook.CapLogRCA {
			continue // the server log, not an extension: a first-look item
		}
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
			"steps; once they are done, Re-check updates this step without a restart. " +
			"Until then query analysis is off."
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
	s.Detail = firstLookDetail(in)
	// A first look none of whose catalog checks ran has looked at nothing;
	// more degraded than counted (an inconsistent report) fails closed too.
	if in.FirstLookDegraded > 0 && in.FirstLookDegraded >= in.FirstLookChecks {
		s.Done = false
		s.Detail = fmt.Sprintf("First look could not run: none of its %d catalog checks "+
			"ran. The first look lists why, with the grant that fixes each.",
			max(in.FirstLookChecks, in.FirstLookDegraded))
		return s
	}
	if in.TTFFSeconds != nil {
		s.Detail += fmt.Sprintf(" First finding %s after start.", seconds(*in.TTFFSeconds))
	}
	return s
}

func firstLookDetail(in ChecklistInput) string {
	var d string
	n := in.FirstLookCounts
	notes := itemNotes(n)
	switch ran := in.FirstLookChecks - in.FirstLookDegraded; {
	case n.Findings > 0:
		d = strings.Join(append([]string{findingsText(n)}, notes...), ", ") +
			" from the catalog."
	case in.FirstLookDegraded > 0:
		d = fmt.Sprintf("No problems found in the %d %s that ran.", ran,
			plural(ran, "check", "checks")) + alsoLists(notes)
	default:
		d = "No problems found in the catalog." + alsoLists(notes) +
			" Query findings follow as statistics accumulate."
	}
	if in.FirstLookDegraded > 0 {
		d = fmt.Sprintf("First look partial: %d of %d checks could not run. %s",
			in.FirstLookDegraded, in.FirstLookChecks, d)
	}
	if n := in.FirstLookTruncated; n > 0 {
		d += fmt.Sprintf(" %d %s only %s first findings.", n,
			plural(n, "check shows", "checks show"), plural(n, "its", "their"))
	}
	return d
}

// findingsText is "3 findings (1 critical)".
func findingsText(n firstlook.ItemCounts) string {
	s := fmt.Sprintf("%d %s", n.Findings, plural(n.Findings, "finding", "findings"))
	if n.Critical > 0 {
		s += fmt.Sprintf(" (%d critical)", n.Critical)
	}
	return s
}

// itemNotes names the items that are not findings, by kind.
func itemNotes(n firstlook.ItemCounts) []string {
	var out []string
	for _, k := range []struct {
		n         int
		one, many string
	}{{n.Notes, "informational item", "informational items"},
		{n.Setup, "setup note", "setup notes"},
		{n.Posture, "posture note", "posture notes"}} {
		if k.n > 0 {
			out = append(out, fmt.Sprintf("%d %s", k.n, plural(k.n, k.one, k.many)))
		}
	}
	return out
}

func alsoLists(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return " The first look also lists " + strings.Join(notes, ", ") + "."
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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
		if in.AutonomyChosen {
			s.Done = true
			s.Detail = "You chose Watch only: pg_sage observes and records what it " +
				"would do; nothing changes outside its sage schema. Grant more any time."
			return s
		}
		s.Detail = "Read-only: pg_sage observes and records what it would do; nothing " +
			"changes outside its sage schema until you grant more."
	default:
		s.Detail = "Trust level unknown."
	}
	return s
}
