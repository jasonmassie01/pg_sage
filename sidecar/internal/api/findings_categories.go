package api

import (
	"net/http"

	"github.com/pg-sage/sidecar/internal/findingcat"
)

// categorySubsystem is one source filter value and the categories it
// matches (a prefix family is listed as its prefix).
type categorySubsystem struct {
	Name       string   `json:"name"`
	Categories []string `json:"categories"`
}

// categoryEntry is one registry entry as the categories route reports it.
type categoryEntry struct {
	Name       string   `json:"name"`
	Prefix     bool     `json:"prefix"`
	Subsystem  string   `json:"subsystem"`
	AlsoIn     []string `json:"also_in"`
	Producer   string   `json:"producer"`
	Severities []string `json:"severities"`
	ConfigKeys []string `json:"config_keys"`
	Actions    []string `json:"actions"`
	Summary    string   `json:"summary"`
}

// findingCategoriesHandler serves GET /api/v1/findings/categories: every
// subsystem the source filter accepts with its categories, and every
// registry entry, so clients build the source filter from the registry.
func findingCategoriesHandler(w http.ResponseWriter, _ *http.Request) {
	subsystems := []categorySubsystem{}
	for _, s := range findingcat.Subsystems() {
		subsystems = append(subsystems, categorySubsystem{Name: s,
			Categories: findingcat.Names(findingcat.BySubsystem(s))})
	}
	entries := []categoryEntry{}
	for _, c := range findingcat.All() {
		entries = append(entries, categoryEntry{Name: c.Name, Prefix: c.Prefix,
			Subsystem: c.Subsystem, AlsoIn: nonNil(c.AlsoIn), Producer: c.Producer,
			Severities: nonNil(c.Severities), ConfigKeys: nonNil(c.ConfigKeys),
			Actions: nonNil(c.Actions), Summary: c.Summary})
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{
		"subsystems": subsystems, "categories": entries})
}

// nonNil returns s, or an empty list for nil, so JSON carries [] not null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
