package config

import "testing"

func TestLakebaseHistoryRequiresExplicitBoundedScope(t *testing.T) {
	valid := LakebaseObservabilityConfig{Enabled: true, WarehouseID: "warehouse-1", Bindings: []LakebaseObservabilityBinding{{Host: "ep-test.database.example.com", Database: "sage_test", Endpoint: "projects/search/branches/test/endpoints/primary"}}}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*LakebaseObservabilityConfig){
		func(c *LakebaseObservabilityConfig) { c.Bindings = nil },
		func(c *LakebaseObservabilityConfig) { c.WarehouseID = "https://foreign" },
		func(c *LakebaseObservabilityConfig) { c.MaxRows = 10001 },
		func(c *LakebaseObservabilityConfig) { c.PollIntervalSeconds = 1 },
		func(c *LakebaseObservabilityConfig) { c.RetentionHours = 169 },
		func(c *LakebaseObservabilityConfig) { c.Bindings[0].Host = "127.0.0.1" },
		func(c *LakebaseObservabilityConfig) { c.Bindings[0].Host = "user@ep-test" },
		func(c *LakebaseObservabilityConfig) { c.Bindings[0].Endpoint = "projects/search" },
		func(c *LakebaseObservabilityConfig) { c.Bindings = append(c.Bindings, c.Bindings[0]) },
		func(c *LakebaseObservabilityConfig) { c.Streams = []string{"plan_history", "plan_history"} },
	} {
		candidate := valid
		candidate.Bindings = append([]LakebaseObservabilityBinding(nil), valid.Bindings...)
		mutate(&candidate)
		if candidate.validate() == nil {
			t.Fatalf("invalid scope/budget accepted: %+v", candidate)
		}
	}
	if _, ok := valid.Binding("EP-TEST.DATABASE.EXAMPLE.COM.", "sage_test"); !ok {
		t.Fatal("exact DNS binding not resolved")
	}
	if _, ok := valid.Binding("ep-test.database.example.com", "other"); ok {
		t.Fatal("cross-database binding accepted")
	}
}
