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
	// Control names the database the lease row lives in.
	Control ControlIdentity
}

// ControlIdentity identifies a control database: its cluster's
// pg_control_system() system_identifier (empty where a managed provider
// restricts that function) and its datname.
type ControlIdentity struct {
	SystemIdentifier string
	Datname          string
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

// WithForeignHolderRefusal makes the elector refuse leadership for the run
// once it sees another live sidecar take or renew the lease; reason says
// why sharing the lease is unsafe. A lease left behind by a sidecar that
// died is not live: it never renews, so it is taken over once it expires.
func WithForeignHolderRefusal(reason string) Option {
	return func(e *Elector) { e.refuseForeign = reason }
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

	refuseForeign string

	mu          sync.Mutex
	refused     string
	lastForeign Lease
	control     ControlIdentity
	held        bool
	wasLeader   bool
	epoch       int64
	deadline    time.Time
	current     string
	lastErr     string
	transitions int
	created     time.Time
	lastRenew   time.Time
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
	e.created = e.now()
	return e
}

// TTL is the lease duration.
func (e *Elector) TTL() time.Duration { return e.ttl }

// Holder is the holder id this elector leads as.
func (e *Elector) Holder() string { return e.holder }

// Tick acquires or renews the lease once. The store call is bounded by
// ttl/3 (pool acquire and every query), so a half-open connection or an
// exhausted pool costs one renewal interval, not the TCP timeout. A
// refused elector makes no store call: it never takes the lease again.
func (e *Elector) Tick(ctx context.Context) error {
	if e.store == nil || e.scope == "" || e.holder == "" {
		return errors.New("leader: elector needs a store, a scope and a holder")
	}
	if e.Refused() {
		return nil
	}
	start := e.now()
	tctx, cancel := context.WithTimeout(ctx, e.ttl/3)
	lease, held, err := e.store.Acquire(tctx, e.scope, e.holder, e.ttl)
	cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.lastErr = err.Error()
		e.noteLapseLocked(err)
		return fmt.Errorf("leader: renew lease of %s: %w", e.scope, err)
	}
	e.noteLapseLocked(errRenewedLate)
	e.lastErr = ""
	e.lastRenew = start
	e.current, e.control = lease.Holder, lease.Control
	e.held = held && e.refused == ""
	if e.held {
		e.epoch, e.lastForeign = lease.Epoch, Lease{}
		e.deadline = start.Add(e.ttl - e.ttl/SafetyDivisor)
	} else if e.refuseForeign != "" && e.foreignLiveLocked(lease) {
		e.refuseLocked(fmt.Sprintf("sidecar %s also holds the lease of %s: %s",
			lease.Holder, e.scope, e.refuseForeign))
	}
	if now := e.leaderLocked(); now != e.wasLeader {
		e.wasLeader = now
		e.transitions++
		e.log("INFO", "leader: %s %s for %s (epoch %d, holder %s)", e.holder,
			map[bool]string{true: "became leader", false: "is a follower"}[now],
			e.scope, lease.Epoch, lease.Holder)
	}
	return nil
}

// foreignLiveLocked reports whether another holder's lease, seen now,
// changed since the last foreign lease this elector saw: a new holder or
// a later expiry means a live sidecar took or renewed it.
func (e *Elector) foreignLiveLocked(l Lease) bool {
	prev := e.lastForeign
	e.lastForeign = l
	return prev.Holder != "" && (prev.Holder != l.Holder || l.ExpiresAt.After(prev.ExpiresAt))
}

// Refuse makes this sidecar a follower for the rest of the run: it never
// takes the lease again. reason is reported by Status and logged once.
func (e *Elector) Refuse(reason string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refuseLocked(reason)
}

func (e *Elector) refuseLocked(reason string) {
	if e.refused != "" {
		return
	}
	e.refused, e.held = reason, false
	e.log("ERROR", "leader: %s refuses leadership of %s for this run: %s", e.holder,
		e.scope, reason)
}

// Refused reports whether this sidecar refused leadership for the run.
func (e *Elector) Refused() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refused != ""
}

// errRenewedLate is the lapse cause when a renewal answered, but only
// after the local deadline had already passed.
var errRenewedLate = errors.New("renewal answered after the local deadline")

// noteLapseLocked counts and logs, once, the moment a sidecar that last
// saw itself as leader finds its local deadline passed.
func (e *Elector) noteLapseLocked(cause error) {
	if !e.wasLeader || e.leaderLocked() {
		return
	}
	e.wasLeader = false
	e.transitions++
	e.log("WARN", "leader: lease of %s lapsed, renewal overdue by %s: %v", e.scope,
		e.now().Sub(e.deadline), cause)
}

func (e *Elector) leaderLocked() bool {
	return e.refused == "" && e.held && e.now().Before(e.deadline)
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

// Token is the lease a fenced write must still hold: the scope the lease
// row is stored under, its holder and its epoch.
type Token struct {
	Scope  string
	Holder string
	Epoch  int64
}

// Fence is the lease this elector holds, scope included, so writers fence
// on the row the elector renews and never on a scope recomputed from
// config. ok is false, with the zero Token, when this sidecar is not the
// leader (or election is disabled).
func (e *Elector) Fence() (Token, bool) {
	if e == nil {
		return Token{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.leaderLocked() {
		return Token{}, false
	}
	return Token{Scope: e.scope, Holder: e.holder, Epoch: e.epoch}, true
}

// Release gives the lease up so a successor takes over at once.
func (e *Elector) Release(ctx context.Context) error {
	e.mu.Lock()
	e.held, e.wasLeader = false, false
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
	LastRenew   time.Time `json:"last_renew,omitempty"`
	// ControlSystemIdentifier is a decimal string: system identifiers
	// exceed the integers JSON readers keep exact.
	ControlSystemIdentifier string `json:"control_system_identifier,omitempty"`
	ControlDatname          string `json:"control_datname,omitempty"`
	LeadershipRefused       bool   `json:"leadership_refused"`
	RefusedReason           string `json:"refused_reason,omitempty"`
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
		Transitions: e.transitions, LastRenew: e.lastRenew,
		ControlSystemIdentifier: e.control.SystemIdentifier,
		ControlDatname:          e.control.Datname,
		LeadershipRefused:       e.refused != "", RefusedReason: e.refused}
	if s.Leader {
		s.Epoch, s.Deadline = e.epoch, e.deadline
	}
	return s
}

// LastRenewAge is the time since the last successful round trip to the
// lease store (renewal or follower read), or since the elector was
// created when none has succeeded yet; zero for a nil elector.
func (e *Elector) LastRenewAge() time.Duration {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	since := e.lastRenew
	if since.IsZero() {
		since = e.created
	}
	return e.now().Sub(since)
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
