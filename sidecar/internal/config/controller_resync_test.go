package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// config-05: a replica adopts the global generation another replica saved
// to the shared control database.

// countingOwner counts every prepare and commit across revisions.
type countingOwner struct {
	mu         sync.Mutex
	name       string
	prepares   int
	commits    int
	prepareErr error
}

func (o *countingOwner) Name() string { return o.name }

func (o *countingOwner) Prepare(
	context.Context, ConfigSnapshot, ConfigSnapshot,
) (PreparedReconfiguration, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prepares++
	if o.prepareErr != nil {
		return nil, o.prepareErr
	}
	return countingPrepared{owner: o}, nil
}

func (o *countingOwner) counts() (int, int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.prepares, o.commits
}

type countingPrepared struct{ owner *countingOwner }

func (p countingPrepared) Commit(context.Context) error {
	p.owner.mu.Lock()
	p.owner.commits++
	p.owner.mu.Unlock()
	return nil
}

func (countingPrepared) Rollback(context.Context) error { return nil }
func (countingPrepared) Drain(context.Context) error    { return nil }

// fakeDurable is a shared control database: a CAS-protected generation and
// the LLM timeout override that revision carries.
type fakeDurable struct {
	mu             sync.Mutex
	generation     uint64
	timeout        int
	generationErr  error
	candidateErr   error
	generationRead int
	candidateRead  int
}

func (d *fakeDurable) DurableGeneration(context.Context) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.generationRead++
	return d.generation, d.generationErr
}

func (d *fakeDurable) DurableCandidate(context.Context) (*Config, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.candidateRead++
	if d.candidateErr != nil {
		return nil, d.candidateErr
	}
	candidate := DefaultConfig()
	candidate.LLM.TimeoutSeconds = d.timeout
	return candidate, nil
}

// cas is SetOverridesCAS: it fails unless expected is the durable generation.
func (d *fakeDurable) cas(expected uint64, timeout int) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.generation != expected {
		return 0, fmt.Errorf("%w: expected %d, durable %d",
			errFakeStoreConflict, expected, d.generation)
	}
	d.generation++
	d.timeout = timeout
	return d.generation, nil
}

func (d *fakeDurable) reads() (int, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.generationRead, d.candidateRead
}

var errFakeStoreConflict = errors.New("fake store generation conflict")

func timeoutCandidate(seconds int) *Config {
	candidate := DefaultConfig()
	candidate.LLM.TimeoutSeconds = seconds
	return candidate
}

func TestResyncAdoptsAheadGenerationThroughOwners(t *testing.T) {
	owner := &countingOwner{name: LLMOwner}
	store := &revisionStoreStub{}
	controller := NewConfigController(DefaultConfig(), store, owner)
	before := time.Now()

	result, adopted, err := controller.Resync(
		context.Background(), 5, timeoutCandidate(91))
	if err != nil || !adopted {
		t.Fatalf("Resync = adopted %v, err %v; want adopted, nil", adopted, err)
	}
	assertGenerations(t, controller, 5, 5)
	if got := controller.Active().Config.LLM.TimeoutSeconds; got != 91 {
		t.Fatalf("active llm.timeout_seconds = %d, want 91", got)
	}
	if !containsPath(result.Applied, "llm.timeout_seconds") ||
		result.ComponentStatus[LLMOwner] != "applied" ||
		result.DesiredGeneration != 5 || result.ActiveGeneration != 5 {
		t.Fatalf("resync result = %+v", result)
	}
	if prepares, commits := owner.counts(); prepares != 1 || commits != 1 {
		t.Fatalf("owner prepares/commits = %d/%d, want 1/1", prepares, commits)
	}
	if store.calls != 0 {
		t.Fatalf("resync persisted %d revisions; the candidate is already durable",
			store.calls)
	}
	if got := controller.LastResync(); got.Before(before) {
		t.Fatalf("LastResync = %v, want at or after %v", got, before)
	}
}

func TestResyncAtOrBehindDesiredChangesNothing(t *testing.T) {
	for _, durable := range []uint64{0, 2, 3} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			owner := &countingOwner{name: LLMOwner}
			controller := NewConfigControllerAtGeneration(
				DefaultConfig(), 3, nil, owner)
			result, adopted, err := controller.Resync(
				context.Background(), durable, timeoutCandidate(91))
			if err != nil || adopted {
				t.Fatalf("Resync(%d) = adopted %v, err %v; want no-op",
					durable, adopted, err)
			}
			assertGenerations(t, controller, 3, 3)
			if result.DesiredGeneration != 3 || result.ActiveGeneration != 3 ||
				len(result.Applied) != 0 {
				t.Fatalf("no-op result = %+v, want generations 3/3", result)
			}
			if got := controller.Active().Config.LLM.TimeoutSeconds; got == 91 {
				t.Fatal("a stale durable revision reached the active config")
			}
			if prepares, _ := owner.counts(); prepares != 0 {
				t.Fatalf("owner prepared %d times for a no-op resync", prepares)
			}
			if !controller.LastResync().IsZero() {
				t.Fatalf("LastResync = %v after a no-op", controller.LastResync())
			}
		})
	}
}

func TestResyncOneAheadIsAdopted(t *testing.T) {
	controller := NewConfigControllerAtGeneration(DefaultConfig(), 3, nil)
	if _, adopted, err := controller.Resync(
		context.Background(), 4, timeoutCandidate(91),
	); err != nil || !adopted {
		t.Fatalf("Resync(4) at desired 3 = adopted %v, err %v", adopted, err)
	}
	if got := controller.Desired().Generation; got != 4 {
		t.Fatalf("desired generation = %d, want 4", got)
	}
}

func TestResyncRejectsInvalidCandidateNamingTheKey(t *testing.T) {
	owner := &countingOwner{name: LLMOwner}
	controller := NewConfigController(DefaultConfig(), nil, owner)
	candidate := timeoutCandidate(91)
	candidate.Collector.IntervalSeconds = 0

	_, adopted, err := controller.Resync(context.Background(), 4, candidate)
	if !errors.Is(err, ErrInvalidCandidate) || adopted {
		t.Fatalf("Resync(invalid) = adopted %v, err %v; want ErrInvalidCandidate",
			adopted, err)
	}
	if !strings.Contains(err.Error(), "collector.interval_seconds") {
		t.Fatalf("error %q does not name collector.interval_seconds", err)
	}
	assertGenerations(t, controller, 1, 1)
	if prepares, _ := owner.counts(); prepares != 0 {
		t.Fatalf("owner prepared %d times for an invalid candidate", prepares)
	}
	if !controller.LastResync().IsZero() {
		t.Fatal("LastResync moved for a rejected candidate")
	}
}

func TestResyncNilCandidateAndNilContext(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	if _, adopted, err := controller.Resync(
		context.Background(), 4, nil,
	); err == nil || adopted {
		t.Fatalf("Resync(nil candidate) = adopted %v, err %v; want an error", adopted, err)
	}
	assertGenerations(t, controller, 1, 1)
	//nolint:staticcheck // a nil context must not panic
	if _, adopted, err := controller.Resync(nil, 2, timeoutCandidate(91)); err != nil || !adopted {
		t.Fatalf("Resync(nil ctx) = adopted %v, err %v", adopted, err)
	}
}

func TestResyncPrepareFailureLeavesControllerUnchanged(t *testing.T) {
	owner := &countingOwner{name: LLMOwner, prepareErr: errors.New("dial llm: refused")}
	controller := NewConfigController(DefaultConfig(), nil, owner)
	_, adopted, err := controller.Resync(context.Background(), 4, timeoutCandidate(91))
	if err == nil || adopted || !strings.Contains(err.Error(), "dial llm: refused") {
		t.Fatalf("Resync = adopted %v, err %v; want the owner error", adopted, err)
	}
	assertGenerations(t, controller, 1, 1)
	if !controller.LastResync().IsZero() {
		t.Fatal("LastResync moved although no revision was adopted")
	}
}

func TestResyncRestartFieldAdvancesDesiredOnly(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	candidate := DefaultConfig()
	candidate.Collector.IntervalSeconds = 120
	result, adopted, err := controller.Resync(context.Background(), 6, candidate)
	if err != nil || !adopted {
		t.Fatalf("Resync = adopted %v, err %v", adopted, err)
	}
	assertGenerations(t, controller, 6, 1)
	if !containsPath(result.PendingRestart, "collector.interval_seconds") ||
		!containsPath(controller.PendingRestart(), "collector.interval_seconds") {
		t.Fatalf("restart-bound field not pending: result %+v", result)
	}
	if controller.LastResync().IsZero() {
		t.Fatal("LastResync did not move although desired adopted generation 6")
	}
}

// State transition: after a resync the CAS accepts the fresh generation
// and still refuses the stale one.
func TestResyncThenLocalApplyUsesFreshGeneration(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil, &countingOwner{name: LLMOwner})
	if _, _, err := controller.Resync(context.Background(), 5, timeoutCandidate(91)); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if _, err := controller.Apply(
		context.Background(), 1, timeoutCandidate(92),
	); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale apply error = %v, want ErrGenerationConflict", err)
	}
	if _, err := controller.Apply(context.Background(), 5, timeoutCandidate(92)); err != nil {
		t.Fatalf("apply with the fresh generation: %v", err)
	}
	assertGenerations(t, controller, 6, 6)
}

func TestResyncFromDurableWithoutSourceIsNoOp(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	_, adopted, err := controller.ResyncFromDurable(context.Background())
	if err != nil || adopted {
		t.Fatalf("ResyncFromDurable(no source) = adopted %v, err %v", adopted, err)
	}
	controller.SetDurableSource(nil)
	ctx := context.Background()
	if _, adopted, err := controller.ResyncFromDurable(ctx); err != nil || adopted {
		t.Fatalf("ResyncFromDurable(nil source) = adopted %v, err %v", adopted, err)
	}
	assertGenerations(t, controller, 1, 1)
}

func TestResyncFromDurableReadsCandidateOnlyWhenAhead(t *testing.T) {
	durable := &fakeDurable{generation: 1, timeout: 91}
	controller := NewConfigController(DefaultConfig(), nil, &countingOwner{name: LLMOwner})
	controller.SetDurableSource(durable)
	ctx := context.Background()

	if _, adopted, err := controller.ResyncFromDurable(ctx); err != nil || adopted {
		t.Fatalf("equal generation = adopted %v, err %v; want no-op", adopted, err)
	}
	if gens, candidates := durable.reads(); gens != 1 || candidates != 0 {
		t.Fatalf("reads at equal generation = %d/%d, want 1 generation, 0 candidates",
			gens, candidates)
	}
	durable.generation = 3
	if _, adopted, err := controller.ResyncFromDurable(ctx); err != nil || !adopted {
		t.Fatalf("ahead generation = adopted %v, err %v", adopted, err)
	}
	assertGenerations(t, controller, 3, 3)
	if got := controller.Active().Config.LLM.TimeoutSeconds; got != 91 {
		t.Fatalf("active timeout = %d, want the durable 91", got)
	}
	if _, candidates := durable.reads(); candidates != 1 {
		t.Fatalf("candidate reads = %d, want 1", candidates)
	}
}

func TestResyncFromDurableGenerationErrorKeepsConfig(t *testing.T) {
	durable := &fakeDurable{generation: 9, generationErr: errors.New("connection refused")}
	controller := NewConfigController(DefaultConfig(), nil)
	controller.SetDurableSource(durable)
	_, adopted, err := controller.ResyncFromDurable(context.Background())
	if err == nil || adopted || !strings.Contains(err.Error(), "connection refused") ||
		!strings.Contains(err.Error(), "durable config generation") {
		t.Fatalf("ResyncFromDurable = adopted %v, err %v; want a generation read error",
			adopted, err)
	}
	assertGenerations(t, controller, 1, 1)
	if _, candidates := durable.reads(); candidates != 0 {
		t.Fatal("candidate was loaded although the generation read failed")
	}
}

func TestResyncFromDurableCandidateErrorKeepsConfig(t *testing.T) {
	durable := &fakeDurable{generation: 4, candidateErr: errors.New("read overrides: timeout")}
	controller := NewConfigController(DefaultConfig(), nil)
	controller.SetDurableSource(durable)
	_, adopted, err := controller.ResyncFromDurable(context.Background())
	if err == nil || adopted || !strings.Contains(err.Error(), "read overrides: timeout") ||
		!strings.Contains(err.Error(), "generation 4") {
		t.Fatalf("ResyncFromDurable = adopted %v, err %v; want the candidate error", adopted, err)
	}
	assertGenerations(t, controller, 1, 1)
	if !controller.LastResync().IsZero() {
		t.Fatal("LastResync moved after a failed candidate load")
	}
}

// Concurrency: a local PUT and another replica's PUT race for generation
// 2, then this replica resyncs. Exactly one revision wins and this
// controller applies it exactly once.
func TestResyncRacingLocalApplyEndsWithOneGeneration(t *testing.T) {
	for i := 0; i < 50; i++ {
		raceResyncAgainstLocalApply(t)
	}
}

func raceResyncAgainstLocalApply(t *testing.T) {
	t.Helper()
	durable := &fakeDurable{generation: 1, timeout: DefaultLLMTimeoutSeconds}
	owner := &countingOwner{name: LLMOwner}
	controller := NewConfigController(DefaultConfig(), nil, owner)
	controller.SetDurableSource(durable)
	var wg sync.WaitGroup
	var localErr, remoteErr error
	wg.Add(3)
	go func() {
		defer wg.Done()
		_, localErr = controller.ApplyWithPersistence(context.Background(), 1,
			timeoutCandidate(61), func(_ context.Context, snap ConfigSnapshot) error {
				generation, err := durable.cas(1, 61)
				if err == nil && generation != snap.Generation {
					return fmt.Errorf("durable %d, controller %d", generation, snap.Generation)
				}
				return err
			})
	}()
	go func() { defer wg.Done(); _, remoteErr = durable.cas(1, 72) }()
	go func() { defer wg.Done(); _, _, _ = controller.ResyncFromDurable(context.Background()) }()
	wg.Wait()
	if _, _, err := controller.ResyncFromDurable(context.Background()); err != nil {
		t.Fatalf("closing resync: %v", err)
	}
	if (localErr == nil) == (remoteErr == nil) {
		t.Fatalf("local err %v, remote err %v: exactly one writer must win", localErr, remoteErr)
	}
	want := 72
	if localErr == nil {
		want = 61
	}
	assertGenerations(t, controller, 2, 2)
	if got := controller.Active().Config.LLM.TimeoutSeconds; got != want {
		t.Fatalf("active timeout = %d, want the winning revision's %d", got, want)
	}
	if _, commits := owner.counts(); commits != 1 {
		t.Fatalf("owner committed %d revisions, want exactly 1", commits)
	}
}
