// Package leader elects one sidecar to run fleet-wide jobs when several
// sidecars share a control database (roadmap phase 3, fleet learning).
//
// A lease row per scope is acquired and renewed with one conditional
// write on the database clock; a new holder bumps the epoch, the fencing
// token writers check. A sidecar counts itself leader only until its local
// deadline, measured from before its last successful acquire and short of
// the lease by a safety margin, so a partitioned or stalled leader stops
// before any successor can acquire the expired lease.
package leader

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Lease is the current lease of a scope.
type Lease struct {
	Holder    string
	Epoch     int64
	ExpiresAt time.Time
}

// Store acquires, renews and releases leases. Acquire returns the lease
// and whether holder now holds it; when not, the lease is the current
// holder's.
type Store interface {
	Acquire(ctx context.Context, scope, holder string, ttl time.Duration) (Lease, bool, error)
	Release(ctx context.Context, scope, holder string) error
}

// DefaultTTL is the lease duration when none is given.
const DefaultTTL = 30 * time.Second

// SafetyDivisor sets the margin: leadership ends ttl/SafetyDivisor before
// the lease would, absorbing clock drift between sidecar and database.
const SafetyDivisor = 5

// Option configures an Elector.
type Option func(*Elector)

// WithClock replaces the elector's clock (tests).
func WithClock(now func() time.Time) Option { return func(e *Elector) { e.now = now } }

// WithLogger sets the elector's logger.
func WithLogger(log func(string, string, ...any)) Option {
	return func(e *Elector) { e.log = log }
}

// Elector holds or follows one scope's lease. A nil *Elector means
// election is disabled: IsLeader is always true.
type Elector struct {
	store  Store
	scope  string
	holder string
	ttl    time.Duration
	now    func() time.Time
	log    func(string, string, ...any)

	mu          sync.Mutex
	held        bool
	epoch       int64
	deadline    time.Time
	current     string
	lastErr     string
	transitions int
}

// NewElector returns an elector for scope as holder; ttl <= 0 is DefaultTTL.
func NewElector(store Store, scope, holder string, ttl time.Duration,
	opts ...Option) *Elector {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	e := &Elector{store: store, scope: scope, holder: holder, ttl: ttl,
		now: time.Now, log: func(string, string, ...any) {}}
	for _, o := range opts {
		o(e)
	}
	return e
}

// TTL is the lease duration.
func (e *Elector) TTL() time.Duration { return e.ttl }

// Tick acquires or renews the lease once.
func (e *Elector) Tick(ctx context.Context) error {
	if e.store == nil || e.scope == "" || e.holder == "" {
		return errors.New("leader: elector needs a store, a scope and a holder")
	}
	start := e.now()
	lease, held, err := e.store.Acquire(ctx, e.scope, e.holder, e.ttl)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.lastErr = err.Error()
		return fmt.Errorf("leader: renew lease of %s: %w", e.scope, err)
	}
	e.lastErr = ""
	e.current = lease.Holder
	was := e.leaderLocked()
	if held {
		e.held, e.epoch = true, lease.Epoch
		e.deadline = start.Add(e.ttl - e.ttl/SafetyDivisor)
	} else {
		e.held = false
	}
	if now := e.leaderLocked(); now != was {
		e.transitions++
		e.log("INFO", "leader: %s %s for %s (epoch %d, holder %s)", e.holder,
			map[bool]string{true: "became leader", false: "is a follower"}[now],
			e.scope, lease.Epoch, lease.Holder)
	}
	return nil
}

func (e *Elector) leaderLocked() bool {
	return e.held && e.now().Before(e.deadline)
}

// IsLeader reports whether this sidecar may run fleet-wide jobs now.
func (e *Elector) IsLeader() bool {
	if e == nil {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.leaderLocked()
}

// Fence is the holder and epoch writes must still hold; ok is false when
// this sidecar is not the leader (or election is disabled).
func (e *Elector) Fence() (string, int64, bool) {
	if e == nil {
		return "", 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.leaderLocked() {
		return "", 0, false
	}
	return e.holder, e.epoch, true
}

// Release gives the lease up so a successor takes over at once.
func (e *Elector) Release(ctx context.Context) error {
	e.mu.Lock()
	e.held = false
	e.mu.Unlock()
	if e.store == nil {
		return nil
	}
	if err := e.store.Release(ctx, e.scope, e.holder); err != nil {
		return fmt.Errorf("leader: release lease of %s: %w", e.scope, err)
	}
	return nil
}

// Run renews the lease every ttl/3 until ctx ends, then releases it.
func (e *Elector) Run(ctx context.Context) {
	ticker := time.NewTicker(e.ttl / 3)
	defer ticker.Stop()
	for {
		if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
			e.log("WARN", "leader: %v", err)
		}
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := e.Release(rctx); err != nil {
				e.log("WARN", "leader: %v", err)
			}
			cancel()
			return
		case <-ticker.C:
		}
	}
}

// Status is the elector's state for the API.
type Status struct {
	Enabled     bool      `json:"enabled"`
	Scope       string    `json:"scope,omitempty"`
	Self        string    `json:"self,omitempty"`
	Holder      string    `json:"holder,omitempty"`
	Leader      bool      `json:"leader"`
	Epoch       int64     `json:"epoch,omitempty"`
	Deadline    time.Time `json:"deadline,omitempty"`
	LeaseTTL    string    `json:"lease_ttl,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Transitions int       `json:"transitions"`
}

// Status reports the elector's state; a nil elector is disabled.
func (e *Elector) Status() Status {
	if e == nil {
		return Status{Leader: true}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Status{Enabled: true, Scope: e.scope, Self: e.holder, Holder: e.current,
		Leader: e.leaderLocked(), LeaseTTL: e.ttl.String(), LastError: e.lastErr,
		Transitions: e.transitions}
	if s.Leader {
		s.Epoch, s.Deadline = e.epoch, e.deadline
	}
	return s
}

// HolderID is a unique holder identity: host, process and a random suffix.
func HolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "sidecar"
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(suffix))
}
