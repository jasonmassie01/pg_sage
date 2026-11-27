package clone

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// ReceiptingProvider is a Provider that writes a receipt before every
// clone it asks its inner provider for, and keeps the receipt current
// through teardown (spec §6.12). A clone holds production data, so a crash
// between Create and Destroy must leave a durable record a reaper acts on.
// Without a usable receipt store it creates nothing (ErrNoReceiptStore).
type ReceiptingProvider struct {
	inner Provider
	store *ReceiptStore
	cfg   ReceiptingConfig

	backoff *CreateBackoff

	mu      sync.Mutex
	live    map[string]int64 // clone id -> receipt id
	held    map[int64]bool   // receipts this process heartbeats
	beating bool
	beatErr error // the last heartbeat's error
}

// ReceiptingConfig says whose receipts a ReceiptingProvider writes.
type ReceiptingConfig struct {
	Adapter string
	Scope   string
	// Deployment returns the deployment id the receipts are written under
	// (read from the receipt store's database).
	Deployment func(context.Context) (string, error)
	Purpose    string
	// TTL is how long after Record a receipt expires; it must outlast the
	// longest use of the clone, or a reaper may destroy it in use.
	TTL time.Duration

	// lab-05a. AllowFullCopy is clone.allow_full_copy: without it a
	// full-copy (or undescribed) provider is asked for nothing.
	AllowFullCopy bool
	// MaxCreatesPerHour is clone.max_creates_per_hour, per deployment and
	// all purposes; 0 is DefaultMaxCreatesPerHour.
	MaxCreatesPerHour int
	// Rate prices the source for the cost label; nil is unpriced.
	Rate *Rate
	// Holder is the holder id receipts name; "" is ProcessHolder().
	Holder string
	// Lifetime is the process's life: heartbeats stop when it ends. nil
	// is context.Background().
	Lifetime context.Context
	// HeartbeatEvery is the heartbeat period; 0 is DefaultHeartbeatEvery.
	HeartbeatEvery time.Duration
	// Backoff is the provider's create backoff; nil is the process-wide
	// one of Adapter and Scope (SharedCreateBackoff).
	Backoff *CreateBackoff
}

// cleanupTimeout bounds a teardown or receipt update that runs on a fresh
// context because the caller's may be cancelled.
const cleanupTimeout = time.Minute

// NewReceiptingProvider wraps inner. store may be nil: Create then refuses
// with ErrNoReceiptStore.
func NewReceiptingProvider(inner Provider, store *ReceiptStore,
	cfg ReceiptingConfig) (*ReceiptingProvider, error) {
	switch {
	case inner == nil:
		return nil, errors.New("receipting clone provider: inner provider is required")
	case cfg.Adapter == "" || cfg.Scope == "":
		return nil, errors.New("receipting clone provider: adapter and scope are required")
	case cfg.Deployment == nil:
		return nil, errors.New("receipting clone provider: deployment source is required")
	case !purposes[cfg.Purpose]:
		return nil, fmt.Errorf("receipting clone provider: unknown purpose %q", cfg.Purpose)
	case cfg.TTL <= 0:
		return nil, errors.New("receipting clone provider: TTL must be positive")
	case cfg.MaxCreatesPerHour < 0 || cfg.HeartbeatEvery < 0:
		return nil, errors.New("receipting clone provider: max creates per hour and " +
			"heartbeat period must not be negative")
	}
	cfg = receiptingDefaults(cfg)
	return &ReceiptingProvider{inner: inner, store: store, cfg: cfg,
		backoff: cfg.Backoff, live: map[string]int64{}, held: map[int64]bool{}}, nil
}

func receiptingDefaults(cfg ReceiptingConfig) ReceiptingConfig {
	if cfg.MaxCreatesPerHour == 0 {
		cfg.MaxCreatesPerHour = DefaultMaxCreatesPerHour
	}
	if cfg.Holder == "" {
		cfg.Holder = ProcessHolder()
	}
	if cfg.Lifetime == nil {
		cfg.Lifetime = context.Background()
	}
	if cfg.HeartbeatEvery == 0 {
		cfg.HeartbeatEvery = DefaultHeartbeatEvery
	}
	if cfg.Backoff == nil {
		cfg.Backoff = SharedCreateBackoff(cfg.Adapter, cfg.Scope)
	}
	return cfg
}

// Create checks the provider's kind against the opt-in and its backoff,
// records a creating receipt with the cost label and this holder, asks the
// inner provider for the clone under the receipt's resource name, then
// marks the receipt ready with the clone's id.
func (p *ReceiptingProvider) Create(ctx context.Context, spec CloneSpec) (Clone, error) {
	capability, err := CheckOptIn(ctx, p.inner, p.cfg.AllowFullCopy)
	if err != nil {
		return Clone{}, err
	}
	if err := p.backoff.Allow(); err != nil {
		return Clone{}, err
	}
	receipt, err := p.record(ctx, capability.Estimate(p.cfg.TTL, p.cfg.Rate))
	if err != nil {
		return Clone{}, err
	}
	p.hold(receipt.ID)
	spec.ResourceName = ResourceName(receipt)
	created, err := p.inner.Create(ctx, spec)
	p.backoff.Observe(err)
	if err != nil {
		p.release(receipt.ID)
		return Clone{}, p.createFailed(receipt, err)
	}
	if _, err := p.store.MarkReady(ctx, receipt.ID, created.ID); err != nil {
		p.release(receipt.ID)
		return Clone{}, p.unrecorded(receipt, created, err)
	}
	p.mu.Lock()
	p.live[created.ID] = receipt.ID
	p.mu.Unlock()
	return created, nil
}

func (p *ReceiptingProvider) record(ctx context.Context, estimate Cost) (Receipt, error) {
	if p.store == nil || p.store.pool == nil {
		return Receipt{}, ErrNoReceiptStore
	}
	deployment, err := p.cfg.Deployment(ctx)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: read deployment id: %w", ErrNoReceiptStore, err)
	}
	name, err := receiptName(p.cfg.Purpose)
	if err != nil {
		return Receipt{}, err
	}
	receipt, err := p.store.RecordLimited(ctx, Receipt{DeploymentID: deployment,
		Adapter: p.cfg.Adapter, Scope: p.cfg.Scope, Name: name, Purpose: p.cfg.Purpose,
		ExpiresAt: time.Now().Add(p.cfg.TTL), HolderID: p.cfg.Holder, Estimate: &estimate},
		p.cfg.MaxCreatesPerHour)
	if errors.Is(err, ErrCreateRateLimited) {
		return Receipt{}, err
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: no clone is created without one: %w",
			ErrNoReceiptStore, err)
	}
	return receipt, nil
}

// createFailed closes the receipt of a create that returned no clone.
func (p *ReceiptingProvider) createFailed(receipt Receipt, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if _, err := p.store.MarkCreateFailed(ctx, receipt.ID, uncertainCreate(cause)); err != nil {
		return errors.Join(cause, fmt.Errorf("close receipt %d of the failed create: %w",
			receipt.ID, err))
	}
	return cause
}

// unrecorded tears down a clone whose receipt could not name it: a clone
// no receipt names must not outlive this call.
func (p *ReceiptingProvider) unrecorded(receipt Receipt, created Clone, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	err := fmt.Errorf("clone %s created but receipt %d could not record it: %w",
		created.ID, receipt.ID, cause)
	if derr := p.inner.Destroy(ctx, created); derr != nil {
		return errors.Join(err, fmt.Errorf("destroy unrecorded clone %s: %w", created.ID,
			derr))
	}
	if _, merr := p.store.MarkCreateFailed(ctx, receipt.ID, false); merr != nil {
		return errors.Join(err, fmt.Errorf("close receipt %d: %w", receipt.ID, merr))
	}
	return err
}

// ErrCloneLeftBehind marks a failed create whose own cleanup failed, so a
// clone may remain (market-03).
var ErrCloneLeftBehind = errors.New("clone: a failed create may have left a clone")

// uncertainCreate reports whether a failed create may still have made a
// clone: the request may have reached the provider before it timed out
// or was cancelled, or the adapter could not delete what it made.
func uncertainCreate(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrCloneLeftBehind) || (errors.As(err, &netErr) && netErr.Timeout())
}

// Destroy tears down a clone this provider created and closes its
// receipt. A teardown that fails leaves the receipt destroying, for the
// reaper to retry once it expires.
func (p *ReceiptingProvider) Destroy(ctx context.Context, target Clone) error {
	p.mu.Lock()
	id, ok := p.live[target.ID]
	delete(p.live, target.ID)
	delete(p.held, id)
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: clone %q was not created by this provider", ErrReceiptNotFound,
			target.ID)
	}
	// Teardown is attempted even when the receipt cannot be moved: removing
	// a copy of production data is always the safe direction.
	_, markErr := p.store.MarkDestroying(ctx, id)
	if err := p.inner.Destroy(ctx, target); err != nil {
		return errors.Join(fmt.Errorf("destroy clone %s (receipt %d left for the reaper): %w",
			target.ID, id, err), markErr)
	}
	if markErr != nil {
		return fmt.Errorf("clone %s destroyed but receipt %d still says ready: %w",
			target.ID, id, markErr)
	}
	if _, err := p.store.MarkDestroyed(ctx, id); err != nil {
		return fmt.Errorf("clone %s destroyed but receipt %d not closed: %w", target.ID, id,
			err)
	}
	return nil
}

// ReceiptNamer is a provider that can name the receipt of a live clone it
// created, so a rehearsal's record links to it (lab-04a).
type ReceiptNamer interface {
	ReceiptOf(cloneID string) (int64, bool)
}

// ReceiptOf is the receipt id of a live clone this provider created; false
// once it is destroyed or for a clone it did not create.
func (p *ReceiptingProvider) ReceiptOf(cloneID string) (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, ok := p.live[cloneID]
	return id, ok
}

// SnapshotAge is the inner provider's.
func (p *ReceiptingProvider) SnapshotAge(ctx context.Context) (time.Duration, error) {
	return p.inner.SnapshotAge(ctx)
}

// receiptName is a unique receipt name, "<purpose>-<random UUID>".
func receiptName(purpose string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate clone receipt name: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%s-%x-%x-%x-%x-%x", purpose, b[0:4], b[4:6], b[6:8], b[8:10],
		b[10:16]), nil
}

var _ Provider = (*ReceiptingProvider)(nil)
