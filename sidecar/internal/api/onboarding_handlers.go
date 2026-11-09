package api

import (
	"context"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/onboarding"
)

// Five-minute time to value: the first-run checklist, the catalog-only
// first look and the guided "grant more" step.

// onboardingDatabase is one database's onboarding state as the API shows it.
type onboardingDatabase struct {
	Name       string
	Connected  bool
	TrustLevel string
	State      *onboarding.State
	FirstLook  *firstlook.ReportSummary
}

// grantTarget tells the UI how trust.level is changed in this mode.
type grantTarget struct {
	Method    string `json:"method"` // "api" or "yaml"
	ConfigURL string `json:"config_url,omitempty"`
	Key       string `json:"key"`
}

// onboardingReader is where the handlers read from (the fleet in
// production, a fake in tests).
type onboardingReader interface {
	// Names resolves the database parameter; false means unknown.
	Names(database string) ([]string, bool)
	// Database returns what it could read before an error (at least the
	// name, the connection status and the trust level) with that error.
	Database(ctx context.Context, name string) (onboardingDatabase, error)
	// FirstLook is the newest full report; nil before the first one.
	FirstLook(ctx context.Context, name string) (*firstlook.Report, error)
	// MCP reports whether MCP over HTTP is on and, for admins, whether an
	// active token exists (nil: not visible to the caller).
	MCP(ctx context.Context) (bool, *bool)
	Notifications(ctx context.Context) *bool
	Guide(ctx context.Context, name string) (onboarding.GuideInput, grantTarget, error)
}

type firstLookSummary struct {
	Ready      bool       `json:"ready"`
	Items      int        `json:"items"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	DurationMS int64      `json:"duration_ms"`
	Summary    string     `json:"summary,omitempty"`
	// Checks, Degraded and Truncated count the catalog checks (agent posture
	// checks are counted in the report's own section).
	Checks    int `json:"checks"`
	Degraded  int `json:"degraded"`
	Truncated int `json:"truncated"`
}

type onboardingEntry struct {
	Database    string            `json:"database"`
	Connected   bool              `json:"connected"`
	TrustLevel  string            `json:"trust_level"`
	InstallKind string            `json:"install_kind,omitempty"`
	TTFFSeconds *float64          `json:"time_to_first_finding_seconds"`
	FirstLook   firstLookSummary  `json:"first_look"`
	Steps       []onboarding.Step `json:"steps"`
	// Error is why this database could not be read (no_runtime,
	// unreachable, read_failed); the other entries are unaffected.
	Error string `json:"error,omitempty"`
}

// resolveNames writes 503/400/404 and returns false when the request
// cannot be served.
func resolveNames(w http.ResponseWriter, r *http.Request,
	reader onboardingReader) ([]string, bool) {
	if reader == nil {
		jsonError(w, "onboarding unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	database, ok := readDatabaseParam(w, r)
	if !ok {
		return nil, false
	}
	names, found := reader.Names(database)
	if !found {
		jsonError(w, "database not found", http.StatusNotFound)
		return nil, false
	}
	return names, true
}

func onboardingHandler(reader onboardingReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names, ok := resolveNames(w, r, reader)
		if !ok {
			return
		}
		mcpOn, token := reader.MCP(r.Context())
		notifications := reader.Notifications(r.Context())
		out := make([]onboardingEntry, len(names))
		readEach(r.Context(), names, func(ctx context.Context, i int, name string) {
			db, err := reader.Database(ctx, name)
			db.Name = name
			code := entryError("read onboarding", name, err)
			if code == entryUnreachable || code == entryNoRuntime {
				db.Connected = false
			}
			out[i] = entryFor(db, mcpOn, token, notifications)
			out[i].Error = code
		})
		jsonResponse(w, map[string]any{"databases": out})
	})
}

func entryFor(db onboardingDatabase, mcpOn bool, token, notifications *bool) onboardingEntry {
	e := onboardingEntry{Database: db.Name, Connected: db.Connected,
		TrustLevel: db.TrustLevel}
	in := onboarding.ChecklistInput{Connected: db.Connected, MCPEnabled: mcpOn,
		MCPToken: token, Notifications: notifications, TrustLevel: db.TrustLevel}
	if db.State != nil {
		e.InstallKind = string(db.State.InstallKind)
		e.TTFFSeconds = db.State.TTFFSeconds
		in.TTFFSeconds = db.State.TTFFSeconds
	}
	if fl := db.FirstLook; fl != nil {
		finished := fl.FinishedAt
		n := fl.Checks
		e.FirstLook = firstLookSummary{Ready: true, Items: fl.Items,
			FinishedAt: &finished, DurationMS: fl.DurationMS, Summary: fl.Summary,
			Checks: n.Total, Degraded: n.Degraded, Truncated: n.Truncated}
		in.FirstLookReady, in.FirstLookItems = true, fl.Items
		in.FirstLookChecks, in.FirstLookDegraded = n.Total, n.Degraded
		in.FirstLookTruncated = n.Truncated
		in.Capabilities = fl.Capabilities
	}
	e.Steps = onboarding.Checklist(in)
	return e
}

type firstLookEntry struct {
	Database string            `json:"database"`
	Report   *firstlook.Report `json:"report"`
	Error    string            `json:"error,omitempty"`
}

func firstLookHandler(reader onboardingReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names, ok := resolveNames(w, r, reader)
		if !ok {
			return
		}
		out := make([]firstLookEntry, len(names))
		readEach(r.Context(), names, func(ctx context.Context, i int, name string) {
			report, err := reader.FirstLook(ctx, name)
			out[i] = firstLookEntry{Database: name, Report: report,
				Error: entryError("read first look", name, err)}
			if err != nil {
				out[i].Report = nil
			}
		})
		jsonResponse(w, map[string]any{"databases": out})
	})
}

func trustGuideHandler(reader onboardingReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names, ok := resolveNames(w, r, reader)
		if !ok {
			return
		}
		if len(names) != 1 {
			jsonError(w, "choose one database (?database=)", http.StatusBadRequest)
			return
		}
		in, target, err := reader.Guide(r.Context(), names[0])
		if err != nil {
			internalError(w, r, "read trust guide", err)
			return
		}
		jsonResponse(w, map[string]any{"database": names[0], "current": in.Current,
			"levels": onboarding.TrustGuide(in), "grant": target,
			"grants": in.Grants})
	})
}
