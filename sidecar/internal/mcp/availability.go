package mcp

import (
	"context"
	"strings"

	"github.com/pg-sage/sidecar/internal/agenttools"
)

// Tool availability (mcp-09): tools/list shows a tool only for the
// databases where it can be served, so an agent does not pick a tool that
// can only answer "unavailable". tools/call is unchanged: a hidden tool
// called directly still gets its typed unavailable error.

// Availability families: what decides whether a family's tools are listed.
const (
	// FamilyAsk is ask_sage: Ask Sage enabled with an enabled model.
	FamilyAsk = "ask"
	// FamilySpecialist is the specialist_* tools: the contract is enabled.
	FamilySpecialist      = "specialist"
	extensionFamilyPrefix = "extension:"
)

// Availability is an optional Backend interface: whether family can serve
// database ("" when the server has no fleet directory). It must be safe
// for concurrent use, and it must answer true when it does not know (a
// capability-read error hides nothing).
type Availability interface {
	Available(ctx context.Context, family, database string) bool
}

// ExtensionFamily is the family of the tools that need extension.
func ExtensionFamily(extension string) string { return extensionFamilyPrefix + extension }

// FamilyExtension is the extension an extension family names.
func FamilyExtension(family string) (string, bool) {
	extension, ok := strings.CutPrefix(family, extensionFamilyPrefix)
	return extension, ok && extension != ""
}

// availabilityFamily is the family that decides whether tool is listed;
// "" means it is always listed.
func availabilityFamily(tool string) string {
	switch {
	case askToolNames[tool]:
		return FamilyAsk
	case specialistToolNames[tool]:
		return FamilySpecialist
	}
	if extension := agenttools.RequiredExtensions[tool]; agentToolNames[tool] &&
		extension != "" {
		return ExtensionFamily(extension)
	}
	return ""
}

// AvailableWhen says, for the tool reference, when tool is listed.
func AvailableWhen(tool string) string {
	family := availabilityFamily(tool)
	if extension, ok := FamilyExtension(family); ok {
		return "the database has the `" + extension + "` extension"
	}
	switch family {
	case FamilyAsk:
		return "Ask Sage is enabled for the database (`ask.enabled`) with an enabled LLM"
	case FamilySpecialist:
		return "the Postgres-specialist contract is enabled"
	}
	return "always"
}

// availableIn is the names among names where tool can be served, and
// whether there is any. A tool without a family, or a backend without
// Availability, is available everywhere; with no names the backend is
// asked once for database "".
func (s *Server) availableIn(ctx context.Context, tool string, names []string) ([]string,
	bool) {
	family := availabilityFamily(tool)
	check, ok := s.backend.(Availability)
	if family == "" || !ok {
		return names, true
	}
	if len(names) == 0 {
		return names, check.Available(ctx, family, "")
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if check.Available(ctx, family, name) {
			out = append(out, name)
		}
	}
	return out, len(out) > 0
}

// listedTools is the tools in registration order that permitted admits
// (nil admits all) and that can be served somewhere among names, each
// with its database enum narrowed to where it can be served.
func (s *Server) listedTools(ctx context.Context, names []string,
	permitted func(tool string) bool) []Tool {
	tools := make([]Tool, 0, len(s.tools))
	for _, tool := range s.tools {
		if permitted != nil && !permitted(tool.Name) {
			continue
		}
		available, ok := s.availableIn(ctx, tool.Name, names)
		if !ok {
			continue
		}
		tools = append(tools, withDatabaseEnum(tool, available))
	}
	return tools
}

// Available reports whether family can serve database: ask_sage and the
// specialist tools need their dependency, then the sidecar's own
// availability decides (none wired: available).
func (backend *ProductionBackend) Available(ctx context.Context, family,
	database string) bool {
	switch {
	case family == FamilyAsk && backend.dependencies.Ask == nil:
		return false
	case family == FamilySpecialist && backend.dependencies.Specialist == nil:
		return false
	case backend.dependencies.Availability == nil:
		return true
	}
	return backend.dependencies.Availability.Available(ctx, family, database)
}
