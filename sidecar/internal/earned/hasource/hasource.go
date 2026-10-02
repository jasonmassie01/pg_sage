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

// Source adapts an internal/ha monitor: every read probes the role, so a
// downgrade sees the role at authorization time.
type Source struct{ monitor *ha.Monitor }

// New wraps m.
func New(m *ha.Monitor) *Source { return &Source{monitor: m} }

var _ earned.HASource = (*Source)(nil)

// HAStatus probes and reports the role, safe mode and last role change.
func (s *Source) HAStatus(ctx context.Context) (earned.HAState, error) {
	if s == nil || s.monitor == nil {
		return earned.HAState{Role: earned.RoleUnknown}, errors.New("no HA monitor")
	}
	s.monitor.Check(ctx)
	return earned.HAState{Role: string(s.monitor.Role()), SafeMode: s.monitor.InSafeMode(),
		LastRoleChange: s.monitor.LastRoleChange()}, nil
}
