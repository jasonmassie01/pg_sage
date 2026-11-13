package config

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
)

// formerOrphanFields named owners that production never registers
// (config-07): they are restart-bound in every mode and must say so.
var formerOrphanFields = []string{
	// alerting.enabled, routes, webhooks and the targets left this list in
	// alerting-06b: the alerting owner, registered in every mode, applies them.
	"alerting.check_interval_seconds",
	"collector.interval_seconds", "analyzer.interval_seconds",
	"auto_explain.collect_interval_seconds",
	"schema_lint.scan_interval_minutes", "tuner.revalidation_interval_hours",
}

func productionOwnerSet() map[string]bool {
	set := map[string]bool{}
	for _, name := range ProductionOwners() {
		set[name] = true
	}
	return set
}

// Every owner the lifecycle metadata names is one production registers.
func TestReconfigureOwnersAreProductionOwners(t *testing.T) {
	owners := productionOwnerSet()
	fields := append(FieldLifecycles(), DatabaseFieldLifecycles()...)
	unknown := map[string][]string{}
	for _, field := range fields {
		if field.Lifecycle != LifecycleReconfigure &&
			field.Lifecycle != LifecycleLivePolicy {
			continue
		}
		if !owners[field.Owner] {
			unknown[field.Owner] = append(unknown[field.Owner], field.Path)
		}
	}
	if len(unknown) != 0 {
		names := make([]string, 0, len(unknown))
		for name := range unknown {
			names = append(names, name)
		}
		sort.Strings(names)
		t.Fatalf("%d owners are named by field metadata but never registered "+
			"in production: %v (fields %v)", len(names), names, unknown)
	}
}

// llm-09 added the process-wide llm_redaction owner, registered in every
// mode next to llm; llm-06 added the briefing owner after it.
func TestProductionOwnersAreTheRegisteredOwners(t *testing.T) {
	got := ProductionOwners()
	want := []string{TrustPolicyOwner, LLMOwner, LLMRedactionOwner, BriefingOwner,
		AlertingOwner, FleetDatabasesOwner}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ProductionOwners() = %v, want %v", got, want)
	}
	if TrustPolicyOwner != "trust_policy" || LLMOwner != "llm" {
		t.Fatalf("owner names changed: %q %q", TrustPolicyOwner, LLMOwner)
	}
	// The result is a copy: a caller cannot widen the production set.
	got[0] = "collector"
	if ProductionOwners()[0] != TrustPolicyOwner {
		t.Fatal("mutating the returned slice changed ProductionOwners")
	}
}

// Boundary: the 11 former orphans that still have no owner are restart
// (briefing.* left the list when llm-06 gave it an owner, and the alerting
// cooldown, quiet hours and timezone when alerting-06 did); the llm
// resource fields and trust ceilings keep their owners.
func TestFormerOrphanFieldsAreRestartWithoutOwner(t *testing.T) {
	for _, path := range formerOrphanFields {
		meta, ok := LookupFieldLifecycle(path)
		if !ok {
			t.Errorf("%s is not a lifecycle field", path)
			continue
		}
		if meta.Lifecycle != LifecycleRestart || meta.Owner != "" {
			t.Errorf("%s = %s/%q, want restart with no owner",
				path, meta.Lifecycle, meta.Owner)
		}
	}
	kept := map[string]FieldLifecycle{
		"llm.model":        {Lifecycle: LifecycleReconfigure, Owner: LLMOwner},
		"llm.api_key":      {Lifecycle: LifecycleReconfigure, Owner: LLMOwner},
		"trust.level":      {Lifecycle: LifecycleLivePolicy, Owner: TrustPolicyOwner},
		"trust.tier3_safe": {Lifecycle: LifecycleLivePolicy, Owner: TrustPolicyOwner},
		"databases":        {Lifecycle: LifecycleReconfigure, Owner: FleetDatabasesOwner},
		"llm.redact_numeric_literals": {
			Lifecycle: LifecycleReconfigure, Owner: LLMRedactionOwner},
		// An llm field that is not an LLM resource stays restart-bound.
		"llm.context_budget_tokens": {Lifecycle: LifecycleRestart},
	}
	for path, want := range kept {
		meta, ok := LookupFieldLifecycle(path)
		if !ok || meta.Lifecycle != want.Lifecycle || meta.Owner != want.Owner {
			t.Errorf("%s = %+v (%v), want %s/%q", path, meta, ok,
				want.Lifecycle, want.Owner)
		}
	}
}

// With every production owner registered, a former orphan still reports
// restart: the fallback is no longer what makes it honest.
func TestFormerOrphansStayRestartWithAllProductionOwners(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil, productionStubs()...)
	for _, path := range formerOrphanFields {
		got, ok := controller.EffectiveLifecycle(path)
		if !ok || got != LifecycleRestart {
			t.Errorf("EffectiveLifecycle(%s) = %q, %v; want restart", path, got, ok)
		}
	}
	if got, _ := controller.EffectiveLifecycle("llm.model"); got != LifecycleReconfigure {
		t.Fatalf("llm.model = %q with the llm owner registered, want reconfigure", got)
	}
}

// State transition: the alerting engine switch notify -> legacy is
// persisted as desired and reported pending restart; no owner is prepared
// and active is unchanged. (alerting-06b: alerting.enabled, which this
// test used to pin, is now reconciled live by the alerting owner, so the
// restart-bound alerting switch is alerting.engine.)
func TestAlertingEngineChangeIsPendingRestart(t *testing.T) {
	events := &eventLog{}
	base := DefaultConfig()
	base.Alerting.Enabled = true
	stubs := productionStubs()
	for _, stub := range stubs {
		stub.(*ownerStub).events = events
	}
	controller := NewConfigController(base, nil, stubs...)
	candidate := controller.Desired().Config
	candidate.Alerting.Engine = AlertingEngineLegacy
	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply alerting.engine=legacy: %v", err)
	}
	if strings.Join(result.PendingRestart, ",") != "alerting.engine" ||
		len(result.Applied) != 0 || len(result.ComponentStatus) != 0 {
		t.Fatalf("result = %+v, want only alerting.engine pending restart", result)
	}
	assertGenerations(t, controller, 2, 1)
	if controller.Active().Config.Alerting.Engine != AlertingEngineNotify ||
		controller.Desired().Config.Alerting.Engine != AlertingEngineLegacy {
		t.Fatal("active must keep the notify engine; desired must hold legacy")
	}
	if len(events.events) != 0 {
		t.Fatalf("owners ran for a restart-bound field: %v", events.events)
	}
}

func TestOwnerNamesListsRegistrationOrder(t *testing.T) {
	empty := NewConfigController(DefaultConfig(), nil)
	if got := empty.OwnerNames(); got == nil || len(got) != 0 {
		t.Fatalf("OwnerNames() without owners = %#v, want empty non-nil", got)
	}
	controller := NewConfigController(DefaultConfig(), nil,
		&ownerStub{name: TrustPolicyOwner}, nil, &ownerStub{name: ""})
	if err := controller.RegisterOwner(&ownerStub{name: LLMOwner}); err != nil {
		t.Fatal(err)
	}
	if err := controller.RegisterOwner(&ownerStub{name: LLMOwner}); err == nil {
		t.Fatal("a duplicate owner registration succeeded")
	}
	got := controller.OwnerNames()
	if strings.Join(got, ",") != "trust_policy,llm" {
		t.Fatalf("OwnerNames() = %v, want [trust_policy llm]", got)
	}
	got[0] = "collector"
	if controller.OwnerNames()[0] != TrustPolicyOwner {
		t.Fatal("OwnerNames returned the controller's own slice")
	}
}

// Concurrent access: OwnerNames is read while owners register.
func TestOwnerNamesIsSafeDuringRegistration(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = controller.RegisterOwner(&ownerStub{name: string(rune('a' + i))})
		}(i)
		go func() {
			defer wg.Done()
			_ = controller.OwnerNames()
		}()
	}
	wg.Wait()
	if got := controller.OwnerNames(); len(got) != 8 {
		t.Fatalf("OwnerNames() after 8 registrations = %v", got)
	}
}

func productionStubs() []ReconfigurationOwner {
	stubs := make([]ReconfigurationOwner, 0, 3)
	for _, name := range ProductionOwners() {
		stubs = append(stubs, &ownerStub{name: name})
	}
	return stubs
}
