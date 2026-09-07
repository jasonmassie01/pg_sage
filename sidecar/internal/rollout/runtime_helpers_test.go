package rollout

import (
	"context"
	"time"
)

type recordingRuntimeEngine struct {
	requests []Request
	result   Result
	err      error
	run      func(context.Context, Request) (Result, error)
}

func (engine *recordingRuntimeEngine) Rollout(
	ctx context.Context, request Request,
) (Result, error) {
	engine.requests = append(engine.requests, request)
	if engine.run != nil {
		return engine.run(ctx, request)
	}
	return engine.result, engine.err
}

type fakeRuntimeEvidenceSource struct {
	items []PriorEvidence
	err   error
}

func (source *fakeRuntimeEvidenceSource) ListPriorEvidence(
	context.Context,
) ([]PriorEvidence, error) {
	return append([]PriorEvidence(nil), source.items...), source.err
}

type staticRuntimeCohort struct {
	instances []Instance
	err       error
}

func (source *staticRuntimeCohort) InstancesForPrior(
	context.Context, Prior,
) ([]Instance, error) {
	return append([]Instance(nil), source.instances...), source.err
}

type fakeRolloutRunStore struct {
	records []RunRecord
	err     error
}

func (store *fakeRolloutRunStore) Create(
	_ context.Context, record RunRecord,
) error {
	if store.err != nil {
		return store.err
	}
	store.records = append(store.records, record)
	return nil
}

func (store *fakeRolloutRunStore) Update(
	_ context.Context, record RunRecord,
) error {
	if store.err != nil {
		return store.err
	}
	store.records = append(store.records, record)
	return nil
}

func (store *fakeRolloutRunStore) Latest(
	context.Context,
) (RunRecord, bool, error) {
	if store.err != nil {
		return RunRecord{}, false, store.err
	}
	if len(store.records) == 0 {
		return RunRecord{}, false, nil
	}
	return store.records[len(store.records)-1], true, nil
}

func (store *fakeRolloutRunStore) states() []string {
	states := make([]string, 0, len(store.records))
	for _, record := range store.records {
		states = append(states, record.State)
	}
	return states
}

func newTestRuntime(
	engine RuntimeEngine,
	evidence RuntimeEvidenceSource,
	instances []Instance,
	policies PolicySet,
	now time.Time,
) *Runtime {
	return newTestRuntimeWithOptions(
		engine, evidence, instances, policies, &fakeRolloutRunStore{}, now,
		RuntimeOptions{MinVerifiedSuccesses: 1, TriggerInterval: time.Hour},
	)
}

func newTestRuntimeWithOptions(
	engine RuntimeEngine,
	evidence RuntimeEvidenceSource,
	instances []Instance,
	policies PolicySet,
	runs RuntimeRunStore,
	now time.Time,
	options RuntimeOptions,
) *Runtime {
	return NewRuntime(RuntimeDependencies{
		Engine: engine, Evidence: evidence,
		Cohort:   &staticRuntimeCohort{instances: instances},
		Policies: policies, Runs: runs, Now: func() time.Time { return now },
	}, options)
}

func runtimeWithStore(
	engine RuntimeEngine, instances []Instance, policies PolicySet,
	runs RuntimeRunStore,
) *Runtime {
	return newTestRuntimeWithOptions(
		engine,
		&fakeRuntimeEvidenceSource{items: []PriorEvidence{
			priorEvidence("prior-source", "success", time.Now().Add(-time.Hour)),
		}},
		instances, policies, runs, time.Now(),
		RuntimeOptions{MinVerifiedSuccesses: 1, TriggerInterval: time.Hour},
	)
}

func fixedRuntimePolicy(canary, maximum int, regression float64) PolicySet {
	return PolicySet{FleetDefault: &Policy{
		CanaryInstances: canary, MaxAffectedInstances: maximum,
		AggregateRegressionLimitPct: regression,
		RequireLocalReverification:  true,
	}}
}

func inheritedPolicySet() PolicySet {
	canary, maximum, regression := 3, 7, float64(9)
	return PolicySet{
		FleetDefault: &Policy{
			CanaryInstances: 1, MaxAffectedInstances: 10,
			AggregateRegressionLimitPct: 20,
			RequireLocalReverification:  true,
		},
		ClassDefaults: map[string]PolicyPatch{
			"production": {CanaryInstances: &canary, MaxAffectedInstances: &maximum},
		},
		TagDefaults: map[string]PolicyPatch{
			"critical": {AggregateRegressionLimitPct: &regression},
		},
	}
}
