package fleet

import "github.com/pg-sage/sidecar/internal/ha"

// applyHASnapshot shows the database's HA role, safe mode and last role
// change from its monitor's snapshot (fleet-04). Without a monitor, or
// once its last check is stale, the role is unknown.
func applyHASnapshot(s *InstanceStatus, m *ha.Monitor) {
	snap := m.Snapshot()
	s.HARole = string(snap.Role)
	s.HASafeMode = snap.SafeMode
	s.HALastChange = nil
	if !snap.LastChange.IsZero() {
		at := snap.LastChange
		s.HALastChange = &at
	}
}
