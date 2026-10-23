//go:build cgo

package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/agentguard/envbind"
)

// CatalogObjects is the minimal D5 checker the broker ships until the
// grants workstream's checker is wired.
func TestCatalogObjects(t *testing.T) {
	f := newFixture(t, envbind.EnvProd)
	f.classifyColumn(t, "people", "api_key", classify.ClassSecret)
	f.classifyColumn(t, "people", "ssn", classify.ClassPII)
	c := CatalogObjects{Targets: fakeTargets{"db": f.target}, Classes: StoreClasses{}}
	ctx := context.Background()
	check := func(env envbind.Env, objs ...decide.Object) error {
		return c.CheckObjects(ctx, f.p, "db", env, objs)
	}
	if err := check(envbind.EnvProd, decide.Object{Schema: f.schema, Relation: "items"},
		decide.Object{Schema: f.schema, Relation: "people", Column: "name"}); err != nil {
		t.Errorf("resolvable clean objects: %v", err)
	}
	if err := check(envbind.EnvDev, decide.Object{Schema: f.schema, Relation: "people",
		Column: "ssn"}); err != nil {
		t.Errorf("pii column in dev (masked there): %v", err)
	}
	denied := []struct {
		env envbind.Env
		obj decide.Object
	}{
		{envbind.EnvProd, decide.Object{Relation: "items"}}, // unresolved on any path
		{envbind.EnvProd, decide.Object{Schema: f.schema, Relation: "missing"}},
		{envbind.EnvProd, decide.Object{Schema: f.schema, Relation: "people", Column: "nope"}},
		{envbind.EnvProd, decide.Object{Schema: f.schema, Relation: "people", Column: "ssn"}},
		{envbind.EnvDev, decide.Object{Schema: f.schema, Relation: "people",
			Column: "api_key"}},
	}
	for _, d := range denied {
		err := check(d.env, d.obj)
		den, ok := agentguard.IsDenied(err)
		if !ok || den.Reason != decide.ReasonClassification {
			t.Errorf("%+v in %s: %v, want denied agent_classification", d.obj, d.env, err)
		}
	}
	if err := c.CheckObjects(ctx, f.p, "unknown", envbind.EnvProd, nil); !errors.Is(err,
		ErrUnknownDatabase) {
		t.Errorf("unknown database: %v", err)
	}
}
