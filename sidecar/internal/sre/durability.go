package sre

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Durability tracks whether coordination metadata is durable (CHECK-16).
// A metadata outage puts it in an explicit degraded state that blocks
// action handoff; only a successful Verify against the store clears it
// (one lucky write is not proof the store is back).
type Durability struct {
	mu     sync.Mutex
	status DurabilityStatus
}

// DurabilityStatus is the guard's explicit state.
type DurabilityStatus struct {
	Degraded bool
	Reason   string
	Since    time.Time
}

// Pinger is a store that can prove it is reachable.
type Pinger interface {
	Ping(ctx context.Context) error
}

// NewDurability returns a healthy guard.
func NewDurability() *Durability { return &Durability{} }

// Observe records a store result. Only a metadata outage degrades the
// guard; caller errors (invalid request, lost lease, budget) do not, and
// a success does not clear a recorded outage.
func (d *Durability) Observe(err error) {
	if !errors.Is(err, ErrMetadataUnavailable) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.status.Degraded {
		d.status = DurabilityStatus{Degraded: true, Reason: "metadata_unavailable",
			Since: time.Now()}
	}
}

// Verify pings the store and clears the degraded state on success.
func (d *Durability) Verify(ctx context.Context, p Pinger) error {
	if err := p.Ping(ctx); err != nil {
		d.Observe(err)
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = DurabilityStatus{}
	return nil
}

// Status returns the current state.
func (d *Durability) Status() DurabilityStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// HandoffAllowed reports whether an action proposal may be handed to the
// policy gate. A missing guard never allows handoff.
func (d *Durability) HandoffAllowed() bool {
	return d != nil && !d.Status().Degraded
}
