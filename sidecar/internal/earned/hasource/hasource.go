// Package hasource adapts the internal/ha monitor to the earned-autonomy
// ledger's HA signal. It is separate so the ledger (which the executor
// imports) does not import internal/ha.
package hasource

import (
	"context"
	"errors"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/ha"
)

// Source reads the database's one HA monitor (fleet-04) and never probes:
// the HA loop keeps its snapshot current and the executor checks it just
// before each cycle. A snapshot the loop stopped refreshing reads as an
// unknown role, which the limiter downgrades.
type Source struct{ monitor *ha.Monitor }

// FromSnapshot reads m's snapshot.
func FromSnapshot(m *ha.Monitor) *Source { return &Source{monitor: m} }

var _ earned.HASource = (*Source)(nil)

// errNoMonitor fails a source without a monitor closed.
var errNoMonitor = errors.New("no HA monitor")

// HAStatus reports the role, safe mode and last role change of the last
// check.
func (s *Source) HAStatus(context.Context) (earned.HAState, error) {
	if s == nil || s.monitor == nil {
		return earned.HAState{Role: earned.RoleUnknown}, errNoMonitor
	}
	snap := s.monitor.Snapshot()
	return earned.HAState{Role: string(snap.Role), SafeMode: snap.SafeMode,
		LastRoleChange: snap.LastChange}, nil
}
