package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// fleetMCPAvailability tells the MCP server where each gated tool family
// can be served (mcp-09): Ask Sage from the process registry, the
// specialist contract from its process pointer, and an extension a tool
// needs from the database's capability evidence. What it does not know
// counts as available, so a capability-read error hides nothing.
type fleetMCPAvailability struct {
	asks    *ask.Registry
	manager *fleet.DatabaseManager
	// specialist reports whether the specialist contract is served; nil
	// means unknown.
	specialist func() bool
}

// newMCPAvailability is the sidecar's availability over its fleet.
func newMCPAvailability(manager *fleet.DatabaseManager) fleetMCPAvailability {
	return fleetMCPAvailability{asks: askServices(), manager: manager,
		specialist: func() bool { return specialistProcess.Load() != nil }}
}

// Available implements mcp.Availability.
func (a fleetMCPAvailability) Available(_ context.Context, family, database string) bool {
	switch family {
	case mcp.FamilyAsk:
		return a.asks.Available(database)
	case mcp.FamilySpecialist:
		return a.specialist == nil || a.specialist()
	}
	if extension, ok := mcp.FamilyExtension(family); ok {
		return a.extensionAvailable(extension, database)
	}
	return true
}

// extensionAvailable is false only when the database's capability
// evidence says extension is not installed or not usable.
func (a fleetMCPAvailability) extensionAvailable(extension, database string) bool {
	var inst *fleet.DatabaseInstance
	switch {
	case a.manager == nil:
		return true
	case database == "":
		inst = mcpInstance(a.manager, nil)
	default:
		inst = a.manager.GetInstance(database)
	}
	if inst == nil {
		return true
	}
	switch inst.SnapshotStatus().Capabilities.Extensions[extension] {
	case "not_installed", "unavailable":
		return false
	}
	return true
}
