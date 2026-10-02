package ha

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Sage SRE M7 (CHECK-40): earned autonomy downgrades while a failover is
// in progress, which includes the cooldown after a single role change,
// not only flapping safe mode. LastRoleChange reports when the observed
// role last changed; the initial detection and probe errors are not
// changes.
func TestLastRoleChangeTracksConfirmedFlipsOnly(t *testing.T) {
	m, clock, _ := newScripted(
		probeResult{inRecovery: false},
		probeResult{err: errors.New("timeout")},
		probeResult{inRecovery: false},
		probeResult{inRecovery: true},
		probeResult{inRecovery: true},
	)
	ctx := context.Background()
	if !m.LastRoleChange().IsZero() {
		t.Fatal("a new monitor reports a role change")
	}
	m.Check(ctx)
	if !m.LastRoleChange().IsZero() {
		t.Fatal("the initial detection is not a role change")
	}
	clock.now = clock.now.Add(time.Minute)
	m.Check(ctx)
	m.Check(ctx)
	if !m.LastRoleChange().IsZero() {
		t.Fatal("a probe error or the same role is not a role change")
	}
	clock.now = clock.now.Add(time.Minute)
	flipAt := clock.now
	m.Check(ctx)
	if got := m.LastRoleChange(); !got.Equal(flipAt) {
		t.Fatalf("LastRoleChange = %v, want %v", got, flipAt)
	}
	clock.now = clock.now.Add(time.Hour)
	m.Check(ctx)
	if got := m.LastRoleChange(); !got.Equal(flipAt) {
		t.Fatalf("a stable check moved LastRoleChange to %v", got)
	}
}
