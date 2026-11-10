package specialist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
)

// The live-investigation bounds while a database's backend is missing (a
// fleet reload, a reconnect, a brief removal): an investigation that cannot
// be read still counts as live, so the bound fails closed, and only a read
// that shows it ended (or the age cap) releases its slot.

// wide may open investigations of both fake databases.
var wide = Identity{TokenID: "tok-wide", Name: "incident bot", Kind: "agent",
	Scopes: []string{"read"}, Databases: []string{"orders", "billing"}, Transport: "http"}

type boundsHarness struct {
	svc    *Service
	orders *fakeBackend
	other  *fakeBackend
	dir    *switchDirectory
	store  *memStore
	clock  *fakeClock
}

func newBoundsHarness(t *testing.T, limits Limits) *boundsHarness {
	t.Helper()
	h := &boundsHarness{orders: newFakeBackend(), other: newFakeBackend(),
		store: newMemStore(), clock: &fakeClock{now: created.Add(5 * time.Minute)}}
	h.dir = newSwitchDirectory(map[string]Backend{"orders": h.orders, "billing": h.other})
	svc, err := NewService(Deps{Directory: h.dir, Store: h.store, Limits: limits,
		KeepIdentifiers: true, Now: h.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	return h
}

func (h *boundsHarness) setNow(at time.Time) {
	h.clock.mu.Lock()
	h.clock.now = at
	h.clock.mu.Unlock()
}

func TestOpen_BackendAbsentKeepsBound(t *testing.T) {
	h := newBoundsHarness(t, DefaultLimits())
	ctx := context.Background()
	for _, f := range []string{"lock_blocking", "wal_retention", "plan_regression"} {
		if _, err := h.svc.Open(ctx, wide, "orders", symptomReq(f)); err != nil {
			t.Fatal(err)
		}
	}
	// orders drops out of the fleet directory (a reload): its three
	// investigations still run, so a 4th open by the token is refused.
	h.dir.setAbsent("orders", true)
	_, err := h.svc.Open(ctx, wide, "billing", symptomReq("lock_blocking"))
	if !errors.Is(err, ErrTooManyInvestigations) ||
		!strings.Contains(err.Error(), "3 live investigations opened by this token") {
		t.Fatalf("4th open while orders is absent: %v", err)
	}
	if calls, terminal := h.store.marks(); calls != 0 || len(terminal) != 0 {
		t.Fatalf("absent backend marked %d records terminal (%v)", calls, terminal)
	}
	if len(h.other.started) != 0 {
		t.Fatalf("billing started %+v over the bound", h.other.started)
	}
	// orders returns and one investigation has concluded: only that one
	// is marked terminal and the next open passes.
	h.dir.setAbsent("orders", false)
	first := h.store.all()[0]
	h.orders.setState(sre.UUID(first.InvestigationID), sre.StateConcluded)
	resp, err := h.svc.Open(ctx, wide, "billing", symptomReq("lock_blocking"))
	if err != nil || !resp.Created {
		t.Fatalf("open after the backend returned: %+v %v", resp, err)
	}
	calls, terminal := h.store.marks()
	if calls != 1 || len(terminal) != 1 || !terminal[first.ID] {
		t.Fatalf("after return: %d marks, terminal %v, want only %s", calls, terminal,
			first.ID)
	}
}

func TestOpen_AgeCapReleasesOnlyUnreadableInvestigations(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOpenPerIdentity = 1
	ctx := context.Background()
	cases := []struct {
		name    string
		age     time.Duration
		absent  bool
		release bool
	}{
		{"unknown just under the cap", limits.MaxLiveAge - time.Second, true, false},
		{"unknown exactly at the cap", limits.MaxLiveAge, true, false},
		{"unknown just over the cap", limits.MaxLiveAge + time.Second, true, true},
		{"readable live far over the cap", 2 * limits.MaxLiveAge, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newBoundsHarness(t, limits)
			if _, err := h.svc.Open(ctx, wide, "orders",
				symptomReq("lock_blocking")); err != nil {
				t.Fatal(err)
			}
			h.dir.setAbsent("orders", tc.absent)
			h.setNow(created.Add(tc.age)) // the record was opened at created
			_, err := h.svc.Open(ctx, wide, "billing", symptomReq("wal_retention"))
			calls, terminal := h.store.marks()
			if tc.release {
				if err != nil || calls != 1 || !terminal["rec-1"] {
					t.Fatalf("aged-out unknown ref: err %v, %d marks %v", err, calls,
						terminal)
				}
				return
			}
			if !errors.Is(err, ErrTooManyInvestigations) || calls != 0 {
				t.Fatalf("ref at age %s (absent=%t): err %v, %d marks", tc.age,
					tc.absent, err, calls)
			}
		})
	}
}

func TestOpen_UnreadableInvestigationCountsAsLive(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOpenPerIdentity = 1
	h := newBoundsHarness(t, limits)
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, wide, "orders", symptomReq("lock_blocking")); err != nil {
		t.Fatal(err)
	}
	h.orders.detailErr = fmt.Errorf("%w: connection reset by peer",
		sre.ErrMetadataUnavailable)
	_, err := h.svc.Open(ctx, wide, "billing", symptomReq("wal_retention"))
	if !errors.Is(err, ErrTooManyInvestigations) {
		t.Fatalf("transport error while counting: %v", err)
	}
	if calls, _ := h.store.marks(); calls != 0 {
		t.Fatalf("a transport error marked %d records terminal", calls)
	}
	// A not-found read is an ended investigation (it was purged).
	h.orders.detailErr = fmt.Errorf("%w: purged", sre.ErrNotFound)
	if _, err := h.svc.Open(ctx, wide, "billing", symptomReq("wal_retention")); err != nil {
		t.Fatalf("after not found: %v", err)
	}
	if calls, terminal := h.store.marks(); calls != 1 || !terminal["rec-1"] {
		t.Fatalf("not found: %d marks %v", calls, terminal)
	}
}

func TestOpen_BoundStoreFailuresAreUnavailable(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOpenPerIdentity = 1
	h := newBoundsHarness(t, limits)
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, wide, "orders", symptomReq("lock_blocking")); err != nil {
		t.Fatal(err)
	}
	h.store.liveErr = errors.New("connection refused")
	_, err := h.svc.Open(ctx, wide, "billing", symptomReq("wal_retention"))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("LiveOpened failure: %v", err)
	}
	h.store.liveErr = nil
	h.store.markErr = errors.New("read-only transaction")
	h.orders.setState(sre.UUID(h.store.all()[0].InvestigationID), sre.StateConcluded)
	_, err = h.svc.Open(ctx, wide, "billing", symptomReq("wal_retention"))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("MarkTerminal failure: %v", err)
	}
	if len(h.other.started) != 0 {
		t.Fatalf("an unavailable bound still started %+v", h.other.started)
	}
}

func TestLiveState_Classification(t *testing.T) {
	h := newBoundsHarness(t, DefaultLimits())
	ctx := context.Background()
	if _, err := h.svc.Open(ctx, wide, "orders", symptomReq("lock_blocking")); err != nil {
		t.Fatal(err)
	}
	id := h.store.all()[0].InvestigationID
	now := h.clock.Now()
	ref := func(db, inv string, opened time.Time) LiveRef {
		return LiveRef{RecordID: "r", Database: db, InvestigationID: inv, OpenedAt: opened}
	}
	cases := []struct {
		name string
		ref  LiveRef
		want liveness
	}{
		{"readable live", ref("orders", id, now), livenessLive},
		{"unknown database", ref("nowhere", id, now), livenessUnknown},
		{"unknown database aged out", ref("nowhere", id, now.Add(-25*time.Hour)),
			livenessEnded},
		// A zero opened time cannot be aged: it stays live (fails closed).
		{"unknown database, zero opened time", ref("nowhere", id, time.Time{}),
			livenessUnknown},
		{"purged investigation", ref("orders", string(sre.NewUUID()), now), livenessEnded},
		{"empty investigation id", ref("orders", "", now), livenessEnded},
	}
	for _, tc := range cases {
		if got := h.svc.liveState(ctx, tc.ref); got != tc.want {
			t.Errorf("%s: liveState = %d, want %d", tc.name, got, tc.want)
		}
	}
	h.orders.setState(sre.UUID(id), sre.StateConcluded)
	if got := h.svc.liveState(ctx, ref("orders", id, now)); got != livenessEnded {
		t.Errorf("concluded: liveState = %d, want ended", got)
	}
}

func TestOpen_FlappingDirectoryNeverExceedsTheTotal(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxOpenPerIdentity, limits.MaxOpenTotal, limits.WritesPerMinute = 3, 3, 100
	h := newBoundsHarness(t, limits)
	h.dir.flap = true
	fams := []string{"lock_blocking", "wal_retention", "plan_regression",
		"replication_lag", "checkpoint_storm", "lwlock_contention",
		"connection_pressure", "temp_file_explosion"}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := wide
			id.TokenID = fmt.Sprintf("tok-%d", i)
			_, _ = h.svc.Open(context.Background(), id, "orders", symptomReq(fams[i%len(fams)]))
		}(i)
	}
	wg.Wait()
	opened := 0
	for _, r := range h.store.all() {
		if r.Kind == KindOpen && r.Created {
			opened++
		}
	}
	if opened == 0 || opened > limits.MaxOpenTotal {
		t.Fatalf("%d investigations created with a flapping directory, total bound %d",
			opened, limits.MaxOpenTotal)
	}
	if calls, _ := h.store.marks(); calls != 0 {
		t.Fatalf("flapping lookups marked %d live investigations terminal", calls)
	}
}
