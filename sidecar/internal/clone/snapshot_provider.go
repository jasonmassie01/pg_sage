package clone

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Snapshot struct {
	ID        string
	CreatedAt time.Time
}

// SnapshotAPI is the provider-specific control-plane boundary implemented by
// RDS/Aurora, Cloud SQL, or AlloyDB adapters.
type SnapshotAPI interface {
	LatestSnapshot(context.Context) (Snapshot, error)
	Restore(context.Context, Snapshot, CloneSpec) (Clone, error)
	Destroy(context.Context, string) error
}

type SnapshotProvider struct {
	api SnapshotAPI
	now func() time.Time
}

func NewSnapshotProvider(api SnapshotAPI, now func() time.Time) (*SnapshotProvider, error) {
	if api == nil {
		return nil, errors.New("snapshot provider API is required")
	}
	if now == nil {
		now = time.Now
	}
	return &SnapshotProvider{api: api, now: now}, nil
}

func (p *SnapshotProvider) Create(ctx context.Context, spec CloneSpec) (Clone, error) {
	snapshot, err := p.api.LatestSnapshot(ctx)
	if err != nil {
		return Clone{}, fmt.Errorf("read latest managed snapshot: %w", err)
	}
	if err := validateSnapshot(snapshot, p.now()); err != nil {
		return Clone{}, err
	}
	target, err := p.api.Restore(ctx, snapshot, spec)
	if err != nil {
		return Clone{}, fmt.Errorf("restore managed snapshot: %w", err)
	}
	if target.ID == "" || target.DSN == "" {
		base := errors.New("managed snapshot restore returned an invalid clone")
		if target.ID == "" {
			return Clone{}, base
		}
		cleanupErr := p.api.Destroy(context.WithoutCancel(ctx), target.ID)
		if cleanupErr != nil {
			return Clone{}, fmt.Errorf("%w; cleanup failed: %v", base, cleanupErr)
		}
		return Clone{}, base
	}
	target.CreatedFrom = snapshot.CreatedAt
	return target, nil
}

func (p *SnapshotProvider) Destroy(ctx context.Context, target Clone) error {
	if target.ID == "" {
		return errors.New("clone ID is required")
	}
	if err := p.api.Destroy(ctx, target.ID); err != nil {
		return fmt.Errorf("destroy managed snapshot clone: %w", err)
	}
	return nil
}

func (p *SnapshotProvider) SnapshotAge(ctx context.Context) (time.Duration, error) {
	snapshot, err := p.api.LatestSnapshot(ctx)
	if err != nil {
		return 0, fmt.Errorf("read latest managed snapshot: %w", err)
	}
	if err := validateSnapshot(snapshot, p.now()); err != nil {
		return 0, err
	}
	return p.now().Sub(snapshot.CreatedAt), nil
}

func validateSnapshot(snapshot Snapshot, now time.Time) error {
	if snapshot.ID == "" || snapshot.CreatedAt.IsZero() {
		return errors.New("managed snapshot identity and timestamp are required")
	}
	if snapshot.CreatedAt.After(now) {
		return errors.New("managed snapshot timestamp is in the future")
	}
	return nil
}

var _ Provider = (*SnapshotProvider)(nil)
