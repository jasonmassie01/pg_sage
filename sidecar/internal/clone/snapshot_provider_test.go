package clone

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSnapshotAPI struct {
	snapshot  Snapshot
	clone     Clone
	err       error
	destroyed string
}

func (f *fakeSnapshotAPI) LatestSnapshot(context.Context) (Snapshot, error) {
	return f.snapshot, f.err
}

func (f *fakeSnapshotAPI) Restore(
	context.Context, Snapshot, CloneSpec,
) (Clone, error) {
	return f.clone, f.err
}

func (f *fakeSnapshotAPI) Destroy(_ context.Context, id string) error {
	f.destroyed = id
	return f.err
}

func TestSnapshotProviderRestoresAndDestroysEphemeralClone(t *testing.T) {
	created := time.Now().Add(-time.Hour)
	api := &fakeSnapshotAPI{
		snapshot: Snapshot{ID: "snap-1", CreatedAt: created},
		clone:    Clone{ID: "scratch-1", DSN: "postgres://scratch.invalid/db"},
	}
	provider, err := NewSnapshotProvider(api, func() time.Time { return created.Add(time.Hour) })
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	clone, err := provider.Create(context.Background(), CloneSpec{IncludeData: true})
	if err != nil || clone.CreatedFrom != created {
		t.Fatalf("clone=%#v err=%v", clone, err)
	}
	if err := provider.Destroy(context.Background(), clone); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if api.destroyed != "scratch-1" {
		t.Fatalf("destroyed = %q", api.destroyed)
	}
}

func TestSnapshotProviderFailsClosedAndCleansPartialRestore(t *testing.T) {
	api := &fakeSnapshotAPI{
		snapshot: Snapshot{ID: "snap-1", CreatedAt: time.Now()},
		clone:    Clone{ID: "partial"},
	}
	provider, _ := NewSnapshotProvider(api, time.Now)

	_, err := provider.Create(context.Background(), CloneSpec{})
	if err == nil || api.destroyed != "partial" {
		t.Fatalf("err=%v destroyed=%q", err, api.destroyed)
	}
	api.err = errors.New("provider unavailable")
	if _, err := provider.SnapshotAge(context.Background()); err == nil {
		t.Fatal("snapshot failure was ignored")
	}
}

var _ Provider = (*SnapshotProvider)(nil)
