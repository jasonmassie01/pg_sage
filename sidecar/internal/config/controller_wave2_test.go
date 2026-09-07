package config

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigControllerSnapshotsAreDeeplyImmutable(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AgentDB.Providers = map[string]AgentDBProviderConfig{
		"aws": {AllowedRegions: []string{"us-east-1"}},
	}
	cfg.Alerting.Webhooks = []WebhookConfig{{
		Name: "ops", Headers: map[string]string{"Authorization": "secret"},
	}}

	controller := NewConfigController(cfg, nil)
	cfg.AgentDB.Providers["aws"].AllowedRegions[0] = "mutated"
	cfg.Alerting.Webhooks[0].Headers["Authorization"] = "mutated"

	first := controller.Active()
	if got := first.Config.AgentDB.Providers["aws"].AllowedRegions[0]; got != "us-east-1" {
		t.Fatalf("published provider region = %q, want detached value", got)
	}
	first.Config.AgentDB.Providers["aws"].AllowedRegions[0] = "reader-mutated"
	first.Config.Alerting.Webhooks[0].Headers["Authorization"] = "reader-mutated"

	second := controller.Active()
	if got := second.Config.AgentDB.Providers["aws"].AllowedRegions[0]; got != "us-east-1" {
		t.Fatalf("reader mutation reached active snapshot: %q", got)
	}
	if got := second.Config.Alerting.Webhooks[0].Headers["Authorization"]; got != "secret" {
		t.Fatalf("reader mutation reached nested map: %q", got)
	}
}

func TestConfigControllerGenerationsAreMonotonicAndCASProtected(t *testing.T) {
	controller := NewConfigController(
		DefaultConfig(), nil, &ownerStub{name: "trust_policy"},
	)
	candidate := controller.Active().Config
	candidate.Trust.Level = "advisory"

	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	assertGenerations(t, controller, 2, 2)
	if result.ActiveGeneration != 2 || !containsPath(result.Applied, "trust.level") {
		t.Fatalf("first result = %+v", result)
	}

	stale := controller.Active().Config
	stale.Trust.Level = "autonomous"
	if _, err := controller.Apply(context.Background(), 1, stale); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale apply error = %v, want ErrGenerationConflict", err)
	}
	assertGenerations(t, controller, 2, 2)

	next := controller.Active().Config
	next.Safety.CPUCeilingPct--
	if _, err := controller.Apply(context.Background(), 2, next); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	assertGenerations(t, controller, 3, 2)
	if got := controller.Active().Config.Safety.CPUCeilingPct; got == next.Safety.CPUCeilingPct {
		t.Fatal("unowned safety setting was falsely published live")
	}
}

func TestConfigControllerUnownedReconfigureFieldIsPendingRestart(t *testing.T) {
	store := &revisionStoreStub{}
	controller := NewConfigController(DefaultConfig(), store)
	candidate := controller.Active().Config
	candidate.Collector.IntervalSeconds++

	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if containsPath(result.Applied, "collector.interval_seconds") {
		t.Fatalf("unowned field falsely reported applied: %+v", result)
	}
	if !containsPath(result.PendingRestart, "collector.interval_seconds") {
		t.Fatalf("pending restart missing collector interval: %+v", result)
	}
	assertGenerations(t, controller, 2, 1)
	if got := controller.Active().Config.Collector.IntervalSeconds; got == candidate.Collector.IntervalSeconds {
		t.Fatalf("unowned interval became active: %d", got)
	}
}

func TestConfigControllerCASAdvancesOnPendingDesiredGeneration(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	candidate := controller.Desired().Config
	candidate.Collector.IntervalSeconds++
	if _, err := controller.Apply(context.Background(), 1, candidate); err != nil {
		t.Fatalf("first pending apply: %v", err)
	}

	stale := controller.Desired().Config
	stale.Analyzer.IntervalSeconds++
	if _, err := controller.Apply(
		context.Background(), 1, stale,
	); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("stale pending apply error = %v, want conflict", err)
	}

	next := controller.Desired().Config
	next.Analyzer.IntervalSeconds++
	if _, err := controller.Apply(context.Background(), 2, next); err != nil {
		t.Fatalf("second pending apply: %v", err)
	}
	assertGenerations(t, controller, 3, 1)
}

func TestConfigControllerPrepareFailureRollsBackEveryPreparedOwner(t *testing.T) {
	events := &eventLog{}
	collector := &ownerStub{name: "collector", events: events}
	alerting := &ownerStub{name: "alerting", events: events, prepareErr: errors.New("boom")}
	store := &revisionStoreStub{events: events}
	controller := NewConfigController(DefaultConfig(), store, collector, alerting)
	candidate := controller.Active().Config
	candidate.Collector.IntervalSeconds++
	candidate.Alerting.CheckIntervalSeconds++

	if _, err := controller.Apply(context.Background(), 1, candidate); err == nil {
		t.Fatal("prepare failure unexpectedly succeeded")
	}
	assertGenerations(t, controller, 1, 1)
	if store.calls != 0 {
		t.Fatalf("persistence calls = %d, want 0", store.calls)
	}
	if got := collector.prepared.counts(); got != [3]int{0, 1, 0} {
		t.Fatalf("collector commit/rollback/drain = %v, want [0 1 0]", got)
	}
}

func TestConfigControllerPersistenceFailureRollsBackWithoutPublication(t *testing.T) {
	owner := &ownerStub{name: "collector"}
	store := &revisionStoreStub{err: errors.New("disk full")}
	controller := NewConfigController(DefaultConfig(), store, owner)
	candidate := controller.Active().Config
	candidate.Collector.IntervalSeconds++

	if _, err := controller.Apply(context.Background(), 1, candidate); err == nil {
		t.Fatal("persistence failure unexpectedly succeeded")
	}
	assertGenerations(t, controller, 1, 1)
	if got := owner.prepared.counts(); got != [3]int{0, 1, 0} {
		t.Fatalf("commit/rollback/drain = %v, want [0 1 0]", got)
	}
}

func TestConfigControllerRequestPersistenceFailureLeavesSnapshotsUnchanged(
	t *testing.T,
) {
	controller := NewConfigController(DefaultConfig(), nil)
	candidate := controller.Active().Config
	candidate.Trust.Level = "advisory"
	persistErr := errors.New("transaction rolled back")

	_, err := controller.ApplyWithPersistence(
		context.Background(), 1, candidate,
		func(context.Context, ConfigSnapshot) error { return persistErr },
	)
	if !errors.Is(err, persistErr) {
		t.Fatalf("apply error = %v, want %v", err, persistErr)
	}
	assertGenerations(t, controller, 1, 1)
	if got := controller.Active().Config.Trust.Level; got == "advisory" {
		t.Fatal("failed persistence published request candidate")
	}
}

func TestConfigControllerRevertingPendingDesiredPersistsNewRevision(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	pending := controller.Desired().Config
	pending.Collector.IntervalSeconds++
	if _, err := controller.Apply(context.Background(), 1, pending); err != nil {
		t.Fatalf("create pending revision: %v", err)
	}

	var persisted ConfigSnapshot
	result, err := controller.ApplyWithPersistence(
		context.Background(), 2, controller.Active().Config,
		func(_ context.Context, snapshot ConfigSnapshot) error {
			persisted = snapshot
			return nil
		},
	)
	if err != nil {
		t.Fatalf("revert pending revision: %v", err)
	}
	if persisted.Generation != 3 {
		t.Fatalf("persisted generation = %d, want 3", persisted.Generation)
	}
	if persisted.Config.Collector.IntervalSeconds !=
		controller.Active().Config.Collector.IntervalSeconds {
		t.Fatal("persisted revision did not cancel the pending value")
	}
	if result.DesiredGeneration != 3 || result.ActiveGeneration != 3 {
		t.Fatalf("revert result = %+v, want desired=3 active=3", result)
	}
	assertGenerations(t, controller, 3, 3)
}

func TestConfigControllerPersistsSameValueAsNewRevision(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	var persisted ConfigSnapshot
	result, err := controller.ApplyWithPersistence(
		context.Background(), 1, controller.Desired().Config,
		func(_ context.Context, snapshot ConfigSnapshot) error {
			persisted = snapshot
			return nil
		},
	)
	if err != nil {
		t.Fatalf("persist same-value revision: %v", err)
	}
	if persisted.Generation != 2 || result.DesiredGeneration != 2 ||
		result.ActiveGeneration != 2 {
		t.Fatalf("persisted/result generations = %d %+v, want 2",
			persisted.Generation, result)
	}
}

func TestConfigControllerDrainFailureIsCommittedWarning(t *testing.T) {
	owner := &ownerStub{name: "collector", drainErr: errors.New("drain timed out")}
	controller := NewConfigController(DefaultConfig(), &revisionStoreStub{}, owner)
	candidate := controller.Active().Config
	candidate.Collector.IntervalSeconds++

	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("committed apply returned failure: %v", err)
	}
	if len(result.Warnings) != 1 ||
		!strings.Contains(result.Warnings[0], "drain timed out") {
		t.Fatalf("warnings = %v, want drain warning", result.Warnings)
	}
	assertGenerations(t, controller, 2, 2)
	if got := controller.Active().Config.Collector.IntervalSeconds; got != candidate.Collector.IntervalSeconds {
		t.Fatalf("committed interval = %d, want %d", got,
			candidate.Collector.IntervalSeconds)
	}
}

func TestConfigControllerCommitFailureIsPersistedPendingWarning(t *testing.T) {
	owner := &ownerStub{name: "collector", commitErr: errors.New("swap failed")}
	controller := NewConfigController(DefaultConfig(), &revisionStoreStub{}, owner)
	candidate := controller.Active().Config
	candidate.Collector.IntervalSeconds++

	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("persisted desired revision returned failure: %v", err)
	}
	if result.DesiredGeneration != 2 || result.ActiveGeneration != 1 {
		t.Fatalf("result generations = %+v, want desired=2 active=1", result)
	}
	if !containsPath(result.PendingRestart, "collector.interval_seconds") ||
		containsPath(result.Applied, "collector.interval_seconds") {
		t.Fatalf("commit failure result = %+v", result)
	}
	if result.ComponentStatus["collector"] != "commit_failed" {
		t.Fatalf("component status = %+v", result.ComponentStatus)
	}
	if len(result.Warnings) != 1 ||
		!strings.Contains(result.Warnings[0], "swap failed") {
		t.Fatalf("warnings = %v, want commit warning", result.Warnings)
	}
	assertGenerations(t, controller, 2, 1)

	owner.commitErr = nil
	retry, err := controller.Apply(
		context.Background(), 2, controller.Desired().Config,
	)
	if err != nil {
		t.Fatalf("retry identical desired revision: %v", err)
	}
	if retry.DesiredGeneration != 3 || retry.ActiveGeneration != 3 ||
		!containsPath(retry.Applied, "collector.interval_seconds") {
		t.Fatalf("retry result = %+v, want applied generation 3", retry)
	}
	assertGenerations(t, controller, 3, 3)
}

func TestConfigControllerStartsAtPersistedGeneration(t *testing.T) {
	controller := NewConfigControllerAtGeneration(DefaultConfig(), 41, nil)
	assertGenerations(t, controller, 41, 41)
	candidate := controller.Desired().Config
	candidate.Trust.Level = "advisory"
	result, err := controller.Apply(context.Background(), 41, candidate)
	if err != nil {
		t.Fatalf("apply after restart: %v", err)
	}
	if result.DesiredGeneration != 42 {
		t.Fatalf("generation = %d, want 42", result.DesiredGeneration)
	}
}

func TestConfigControllerWaitsForOwnerCommitAcknowledgement(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	owner := &ownerStub{name: "collector", commitGate: gate, commitStarted: started}
	controller := NewConfigController(DefaultConfig(), &revisionStoreStub{}, owner)
	candidate := controller.Active().Config
	candidate.Collector.IntervalSeconds++
	resultCh := make(chan ApplyResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := controller.Apply(context.Background(), 1, candidate)
		resultCh <- result
		errCh <- err
	}()

	waitClosed(t, started, "owner commit")
	assertGenerations(t, controller, 2, 1)
	select {
	case <-resultCh:
		t.Fatal("apply returned before owner acknowledged commit")
	default:
	}
	close(gate)
	result := <-resultCh
	if err := <-errCh; err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.ComponentStatus["collector"] != "applied" {
		t.Fatalf("component status = %+v", result.ComponentStatus)
	}
	assertGenerations(t, controller, 2, 2)
	if got := owner.prepared.counts(); got != [3]int{1, 0, 1} {
		t.Fatalf("commit/rollback/drain = %v, want [1 0 1]", got)
	}
}

func TestConfigControllerTrustWaitsForPolicyOwnerBeforePublication(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	owner := &ownerStub{
		name: "trust_policy", commitGate: gate, commitStarted: started,
	}
	controller := NewConfigController(DefaultConfig(), nil, owner)
	candidate := controller.Active().Config
	candidate.Trust.Level = "advisory"
	done := make(chan error, 1)
	go func() {
		_, err := controller.Apply(context.Background(), 1, candidate)
		done <- err
	}()

	waitClosed(t, started, "trust policy commit")
	if got := controller.Active().Config.Trust.Level; got == "advisory" {
		t.Fatal("trust became active before executor owner acknowledged it")
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("apply trust: %v", err)
	}
	if got := controller.Active().Config.Trust.Level; got != "advisory" {
		t.Fatalf("active trust = %q, want advisory", got)
	}
}

func TestFieldLifecycleRegistryAndEffectiveFallback(t *testing.T) {
	tests := []struct {
		path      string
		lifecycle ConfigLifecycle
		owner     string
	}{
		{"trust.level", LifecycleLivePolicy, "trust_policy"},
		{"collector.interval_seconds", LifecycleReconfigure, "collector"},
		{"alerting.routes", LifecycleReconfigure, "alerting"},
		{"mode", LifecycleRestart, ""},
		{"postgres.database_url", LifecycleRestart, ""},
		{"databases", LifecycleAPI, ""},
	}
	for _, tt := range tests {
		meta, ok := LookupFieldLifecycle(tt.path)
		if !ok || meta.Lifecycle != tt.lifecycle || meta.Owner != tt.owner {
			t.Errorf("LookupFieldLifecycle(%q) = %+v, %v", tt.path, meta, ok)
		}
	}
	if _, ok := LookupFieldLifecycle("collector.interval_second"); ok {
		t.Fatal("unknown key was classified")
	}
	withoutOwner := NewConfigController(DefaultConfig(), nil)
	if got, _ := withoutOwner.EffectiveLifecycle("collector.interval_seconds"); got != LifecycleRestart {
		t.Fatalf("unowned effective lifecycle = %q, want restart", got)
	}
	if got, _ := withoutOwner.EffectiveLifecycle("trust.level"); got != LifecycleRestart {
		t.Fatalf("unowned trust lifecycle = %q, want restart", got)
	}
}

func assertGenerations(t *testing.T, controller *ConfigController, desired, active uint64) {
	t.Helper()
	gotDesired := controller.Desired()
	gotActive := controller.Active()
	if gotDesired.Generation != desired || gotActive.Generation != active {
		t.Fatalf("desired/active generations = %d/%d, want %d/%d",
			gotDesired.Generation, gotActive.Generation, desired, active)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

type revisionStoreStub struct {
	mu     sync.Mutex
	err    error
	calls  int
	events *eventLog
}

func (s *revisionStoreStub) PersistDesired(_ context.Context, snap ConfigSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.events != nil {
		s.events.add("persist")
	}
	return s.err
}

type ownerStub struct {
	name          string
	events        *eventLog
	prepareErr    error
	commitGate    <-chan struct{}
	commitStarted chan struct{}
	commitErr     error
	drainErr      error
	prepared      *preparedStub
}

func (o *ownerStub) Name() string { return o.name }

func (o *ownerStub) Prepare(
	_ context.Context, _, _ ConfigSnapshot,
) (PreparedReconfiguration, error) {
	if o.events != nil {
		o.events.add("prepare:" + o.name)
	}
	if o.prepareErr != nil {
		return nil, o.prepareErr
	}
	o.prepared = &preparedStub{
		name: o.name, events: o.events, commitGate: o.commitGate,
		commitStarted: o.commitStarted, commitErr: o.commitErr,
		drainErr: o.drainErr,
	}
	return o.prepared, nil
}

type preparedStub struct {
	mu            sync.Mutex
	name          string
	events        *eventLog
	commitGate    <-chan struct{}
	commitStarted chan struct{}
	startOnce     sync.Once
	commit        int
	commitErr     error
	rollback      int
	drain         int
	drainErr      error
}

func (p *preparedStub) Commit(ctx context.Context) error {
	p.startOnce.Do(func() {
		if p.commitStarted != nil {
			close(p.commitStarted)
		}
	})
	if p.commitGate != nil {
		select {
		case <-p.commitGate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	p.commit++
	p.mu.Unlock()
	return p.commitErr
}

func (p *preparedStub) Rollback(context.Context) error {
	p.mu.Lock()
	p.rollback++
	p.mu.Unlock()
	return nil
}

func (p *preparedStub) Drain(context.Context) error {
	p.mu.Lock()
	p.drain++
	p.mu.Unlock()
	return p.drainErr
}

func (p *preparedStub) counts() [3]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return [3]int{p.commit, p.rollback, p.drain}
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(event string) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}
