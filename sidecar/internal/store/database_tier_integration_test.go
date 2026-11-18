//go:build integration

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fleetpol-01d: observe-only members live past the 50-member cap of full
// members, up to 2,000, and a tier change counts against the cap of the
// tier it moves to.

func tierInput(name string) DatabaseInput {
	in := validInput()
	in.Name = name
	return in
}

// fillTier inserts n members of tier straight into sage.databases (the
// cap is what is tested, not 2,000 password seals).
func fillTier(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tier string, n int) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.databases
		(name, host, database_name, username, password_enc, tier)
		SELECT 'fill-' || $1 || '-' || g, 'h', 'd', 'u', '\x00'::bytea, $1
		FROM generate_series(1, $2::int) g`, tier, n); err != nil {
		t.Fatalf("fill %d %s members: %v", n, tier, err)
	}
}

func TestObserveOnlyCreate_2000AcceptedAnd2001Refused(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	fillTier(t, ctx, pool, TierObserveOnly, MaxObserveOnlyMembers-1)
	id, err := ds.CreateObserveOnly(ctx, tierInput("obs-2000"), 1)
	if err != nil {
		t.Fatalf("the 2,000th observe-only member: %v", err)
	}
	rec, err := ds.Get(ctx, id)
	if err != nil || rec.Tier != TierObserveOnly || rec.RunsRuntime() {
		t.Fatalf("member %d: %+v (%v), want observe_only without a runtime", id, rec, err)
	}
	_, err = ds.CreateObserveOnly(ctx, tierInput("obs-2001"), 1)
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(),
		"maximum of 2000 observe-only databases reached") {
		t.Fatalf("the 2,001st: %v, want the observe-only cap", err)
	}
	if n, err := ds.CountTier(ctx, TierObserveOnly); err != nil || n != 2000 {
		t.Fatalf("observe-only count %d (%v), want 2000", n, err)
	}
	// Observe-only members never use up the full members' 50.
	if _, err := ds.Create(ctx, tierInput("full-1"), 1); err != nil {
		t.Fatalf("a full member next to 2,000 observe-only ones: %v", err)
	}
	if n, err := ds.Count(ctx); err != nil || n != 1 {
		t.Fatalf("Count (full members) = %d (%v), want 1", n, err)
	}
}

func TestFullCap_UnchangedAt50(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	fillTier(t, ctx, pool, TierFull, MaxFullMembers-1)
	if _, err := ds.Create(ctx, tierInput("full-50"), 1); err != nil {
		t.Fatalf("the 50th full member: %v", err)
	}
	_, err := ds.Create(ctx, tierInput("full-51"), 1)
	if !errors.Is(err, ErrValidation) ||
		!strings.Contains(err.Error(), "maximum of 50 databases reached") {
		t.Fatalf("the 51st full member: %v, want the 50 cap", err)
	}
	if _, err := ds.CreateObserveOnly(ctx, tierInput("obs-1"), 1); err != nil {
		t.Fatalf("an observe-only member past 50 full ones: %v", err)
	}
}

func TestSetTier_ObserveOnlyToFullToDisabled(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	id, err := ds.CreateObserveOnly(ctx, tierInput("obs-a"), 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	listed, err := ds.ObserveOnlyMembers(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != id {
		t.Fatalf("observe-only members %+v (%v), want [%d]", listed, err, id)
	}
	if err := ds.SetTier(ctx, id, TierFull); err != nil {
		t.Fatalf("promote: %v", err)
	}
	rec, _ := ds.Get(ctx, id)
	if rec.Tier != TierFull || !rec.RunsRuntime() {
		t.Fatalf("promoted member %+v, want a full member with a runtime", rec)
	}
	if listed, _ := ds.ObserveOnlyMembers(ctx); len(listed) != 0 {
		t.Fatalf("a full member is still swept: %+v", listed)
	}
	if err := ds.SetTier(ctx, id, TierFull); err != nil {
		t.Fatalf("promoting a full member again: %v, want no-op", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.databases SET enabled = false
		WHERE id = $1`, id); err != nil {
		t.Fatalf("disable: %v", err)
	}
	rec, _ = ds.Get(ctx, id)
	if rec.RunsRuntime() {
		t.Fatal("a disabled member runs a runtime")
	}
	if err := ds.SetTier(ctx, id, TierObserveOnly); err != nil {
		t.Fatalf("demote: %v", err)
	}
	if listed, _ := ds.ObserveOnlyMembers(ctx); len(listed) != 0 {
		t.Fatalf("a disabled observe-only member is swept: %+v", listed)
	}
}

func TestSetTier_InvalidInputAndMissingMember(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	id, err := ds.CreateObserveOnly(ctx, tierInput("obs-b"), 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.SetTier(ctx, id, "light"); !errors.Is(err, ErrValidation) {
		t.Fatalf("tier light: %v, want ErrValidation", err)
	}
	if err := ds.SetTier(ctx, id+1000, TierFull); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing member: %v, want ErrNotFound", err)
	}
	if _, err := ds.CreateObserveOnly(ctx, DatabaseInput{}, 1); !errors.Is(err,
		ErrValidation) {
		t.Fatalf("an empty input: %v, want ErrValidation", err)
	}
}

func TestSetTier_PromotionAtTheFullCapIsRefused(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	id, err := ds.CreateObserveOnly(ctx, tierInput("obs-c"), 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fillTier(t, ctx, pool, TierFull, MaxFullMembers)
	err = ds.SetTier(ctx, id, TierFull)
	if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(),
		"maximum of 50 databases reached") {
		t.Fatalf("promotion past 50: %v, want the 50 cap", err)
	}
	if rec, _ := ds.Get(ctx, id); rec.Tier != TierObserveOnly {
		t.Fatalf("refused promotion changed the tier to %q", rec.Tier)
	}
}

// Ten sidecars promote ten observe-only members at once with room for
// five: exactly five are promoted and the cap holds.
func TestSetTier_ConcurrentPromotionsKeepTheCap(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	fillTier(t, ctx, pool, TierFull, MaxFullMembers-5)
	ids := make([]int, 10)
	for i := range ids {
		id, err := ds.CreateObserveOnly(ctx, tierInput(fmt.Sprintf("obs-%d", i)), 1)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids[i] = id
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, capped := 0, 0
	for _, id := range ids {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			err := ds.SetTier(ctx, id, TierFull)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrValidation):
				capped++
			default:
				t.Errorf("promote %d: %v", id, err)
			}
		}(id)
	}
	wg.Wait()
	if ok != 5 || capped != 5 {
		t.Fatalf("%d promoted, %d refused; want 5 and 5", ok, capped)
	}
	if n, _ := ds.Count(ctx); n != MaxFullMembers {
		t.Fatalf("full members %d, want %d", n, MaxFullMembers)
	}
}

func TestObserveSweeps_RecordAndRound(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	id, err := ds.CreateObserveOnly(ctx, tierInput("obs-d"), 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	start := time.Now().UTC().Truncate(time.Millisecond)
	round, err := ds.BeginObserveRound(ctx, start, 1)
	if err != nil || round <= 0 {
		t.Fatalf("begin round: %d (%v)", round, err)
	}
	sw := ObserveSweep{DatabaseID: id, RoundID: round, SweptAt: start,
		Status: ObserveOK, Letter: "B", Grade: []byte(`{"overall":"B"}`), DurationMS: 12}
	if err := ds.RecordObserveSweep(ctx, sw); err != nil {
		t.Fatalf("record: %v", err)
	}
	sw.Status, sw.Error, sw.Letter, sw.Grade = ObserveUnreachable, "dial: refused", "", nil
	if err := ds.RecordObserveSweep(ctx, sw); err != nil {
		t.Fatalf("record again: %v", err)
	}
	got, err := ds.ObserveSweeps(ctx)
	if err != nil || len(got) != 1 || got[0].Status != ObserveUnreachable ||
		got[0].Error != "dial: refused" || got[0].Letter != "" {
		t.Fatalf("sweeps %+v (%v), want the newest (unreachable) only", got, err)
	}
	bad := sw
	bad.Status = "graded"
	if err := ds.RecordObserveSweep(ctx, bad); !errors.Is(err, ErrValidation) {
		t.Fatalf("status graded: %v, want ErrValidation", err)
	}
	fin := ObserveRound{ID: round, StartedAt: start, FinishedAt: start.Add(time.Second),
		Members: 1, Unreachable: 1, MaxConcurrent: 1}
	if err := ds.FinishObserveRound(ctx, fin); err != nil {
		t.Fatalf("finish: %v", err)
	}
	last, ok, err := ds.LatestObserveRound(ctx)
	if err != nil || !ok || last.ID != round || last.Unreachable != 1 ||
		last.MaxConcurrent != 1 || !last.FinishedAt.Equal(fin.FinishedAt) {
		t.Fatalf("latest round %+v ok=%v (%v)", last, ok, err)
	}
}

func TestObserveSweeps_EmptyFleet(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	if _, err := pool.Exec(ctx, `DELETE FROM sage.observe_rounds`); err != nil {
		t.Fatalf("clean rounds: %v", err)
	}
	ds := NewDatabaseStore(pool, testKey)
	if m, err := ds.ObserveOnlyMembers(ctx); err != nil || len(m) != 0 {
		t.Fatalf("members %+v (%v), want none", m, err)
	}
	if _, ok, err := ds.LatestObserveRound(ctx); err != nil || ok {
		t.Fatalf("latest round ok=%v (%v), want none", ok, err)
	}
}

func TestObserveSweeps_CanceledContextPropagates(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ds.ObserveOnlyMembers(cctx); err == nil ||
		!strings.Contains(err.Error(), "listing observe-only databases") {
		t.Fatalf("canceled list: %v, want a wrapped error", err)
	}
	if err := ds.SetTier(cctx, 1, TierFull); err == nil {
		t.Fatal("canceled SetTier succeeded")
	}
}

func TestObserveSweepByName_ObserveOnlyMembersOnly(t *testing.T) {
	pool, ctx := requireDBWithClean(t)
	ds := NewDatabaseStore(pool, testKey)
	id, err := ds.CreateObserveOnly(ctx, tierInput("obs-e"), 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ds.Create(ctx, tierInput("full-e"), 1); err != nil {
		t.Fatalf("create full: %v", err)
	}
	sw, ok, err := ds.ObserveSweepByName(ctx, "obs-e")
	if err != nil || !ok || sw != nil {
		t.Fatalf("not swept yet: %+v ok=%v (%v), want observe-only with no sweep", sw, ok,
			err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := ds.RecordObserveSweep(ctx, ObserveSweep{DatabaseID: id, RoundID: 1,
		SweptAt: at, Status: ObserveOK, Letter: "C",
		Grade: []byte(`{"overall":"C"}`)}); err != nil {
		t.Fatalf("record: %v", err)
	}
	sw, ok, err = ds.ObserveSweepByName(ctx, "obs-e")
	if err != nil || !ok || sw == nil || sw.Letter != "C" || !sw.SweptAt.Equal(at) ||
		sw.DatabaseID != id {
		t.Fatalf("swept: %+v ok=%v (%v)", sw, ok, err)
	}
	for _, name := range []string{"full-e", "nope", ""} {
		if sw, ok, err := ds.ObserveSweepByName(ctx, name); err != nil || ok || sw != nil {
			t.Errorf("%q: %+v ok=%v (%v), want not observe-only", name, sw, ok, err)
		}
	}
}
