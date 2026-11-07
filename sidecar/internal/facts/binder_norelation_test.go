package facts

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
)

// facts-01 review: a request whose targets name no relation (the executor
// marks configuration, setting, database, instance and query findings)
// is matched only by its SQL and its slot targets, with or without a
// resolver, and asks the catalog nothing about those targets.

// No concurrent access tests here: the flag only filters the request's
// own targets; concurrent binds are covered in binder_targets_test.go.

func noRelationRequest(sql string, targets ...string) policy.ActionRequest {
	req := configRequest(sql, targets...)
	req.TargetsNameNoRelation = true
	return req
}

func TestBinderNoRelationTargetsNeverBindWithoutResolver(t *testing.T) {
	b := NewBinder(staticFacts(fixtureFacts()), nil, func() time.Time { return bindNow })
	for _, target := range []string{"instance", "lifeos", "work_mem", "orders"} {
		got, err := b.Bind(context.Background(),
			noRelationRequest("ALTER SYSTEM SET work_mem='64MB'", target))
		if err != nil || got != nil {
			t.Fatalf("target %q: bindings %+v err %v, want none", target, got, err)
		}
	}
	// The same target without the mark is matched as written (conservative).
	got, err := b.Bind(context.Background(),
		configRequest("ALTER SYSTEM SET work_mem='64MB'", "instance"))
	if err != nil || !reflect.DeepEqual(bindingIDs(got), []int64{1, 2}) {
		t.Fatalf("unmarked instance target: %+v err %v, want facts 1 and 2", got, err)
	}
}

func TestBinderNoRelationTargetsAskTheCatalogNothing(t *testing.T) {
	cat := &fakeCatalog{err: errors.New("catalog must not be read")}
	got, err := targetBinder(cat).Bind(context.Background(),
		noRelationRequest("ALTER SYSTEM SET work_mem='64MB'", "orders"))
	if err != nil || got != nil || len(cat.asked) != 0 {
		t.Fatalf("bindings %+v err %v asked %v, want none, nil, no lookup", got, err,
			cat.asked)
	}
}

func TestBinderNoRelationKeepsSQLRefsAndSlotTargets(t *testing.T) {
	src := staticFacts(append(fixtureFacts(),
		confirmed(3, TypeSlotConsumer, KindSlot, "cdc_*", nil)))
	b := NewBinder(src, nil, func() time.Time { return bindNow })
	got, err := b.Bind(context.Background(),
		noRelationRequest("ANALYZE test_a.orders", "instance"))
	if err != nil || !reflect.DeepEqual(bindingIDs(got), []int64{1}) ||
		got[0].Object != "test_a.orders" {
		t.Fatalf("SQL-named fixture table: %+v err %v, want fact 1 on test_a.orders", got,
			err)
	}
	got, err = b.Bind(context.Background(),
		noRelationRequest("ALTER SYSTEM SET wal_level='logical'", "slot:cdc_main"))
	if err != nil || !reflect.DeepEqual(bindingIDs(got), []int64{3}) {
		t.Fatalf("slot target: %+v err %v, want slot fact 3", got, err)
	}
}

func bindingIDs(bs []policy.FactBinding) []int64 {
	var out []int64
	for _, b := range bs {
		out = append(out, b.FactID)
	}
	return out
}
