package fleet

// DatabaseFacets are the registration facts the fleet list (ux-10a)
// searches and sorts on that a status row does not carry. They are read
// from the config only and never put in the status response.
type DatabaseFacets struct {
	Host          string
	ExecutionMode string
}

// FleetListing returns FleetStatus and each listed database's facets by
// name, read under one lock so the rows and the facets agree.
func (m *DatabaseManager) FleetListing() (FleetOverview, map[string]DatabaseFacets) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	facets := make(map[string]DatabaseFacets, len(m.instances))
	for _, inst := range m.instances {
		mode := inst.Config.ExecutionMode
		if mode == "" {
			// Config.normalize resolves an unset mode to auto the same way.
			mode = "auto"
		}
		facets[inst.Name] = DatabaseFacets{Host: inst.Config.Host, ExecutionMode: mode}
	}
	return m.fleetStatusLocked(), facets
}
