package probes

import (
	"errors"
	"testing"
	"time"
)

// CheckArgs (Sage SRE M3): a model-proposed next probe is validated
// before anything runs. The id must be a catalog probe and the typed
// arguments must fit that probe's argument kind and bounds.

func TestCheckArgs_AcceptsTypedArgsPerKind(t *testing.T) {
	reg := Catalog()
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ok := []struct {
		id   ID
		args Args
	}{
		{LockGraph, Args{}},
		{PreparedXacts, Args{}},
		{BackendIdentity, Args{PID: 4242, BackendStart: start}},
		{SageActions, Args{}},
		{SageActions, Args{Window: MinWindow}},
		{SageActions, Args{Window: MaxWindow}},
		{PlanRegressions, Args{Window: time.Hour}},
	}
	for _, c := range ok {
		if err := reg.CheckArgs(c.id, c.args); err != nil {
			t.Errorf("CheckArgs(%s, %+v) = %v, want nil", c.id, c.args, err)
		}
	}
}

func TestCheckArgs_RejectsWrongArgs(t *testing.T) {
	reg := Catalog()
	start := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	bad := []struct {
		name string
		id   ID
		args Args
	}{
		{"pid on a no-arg probe", LockGraph, Args{PID: 5}},
		{"window on a no-arg probe", LockGraph, Args{Window: time.Hour}},
		{"backend without start", BackendIdentity, Args{PID: 5}},
		{"backend without pid", BackendIdentity, Args{BackendStart: start}},
		{"negative pid", BackendIdentity, Args{PID: -1, BackendStart: start}},
		{"backend with window", BackendIdentity, Args{PID: 5, BackendStart: start,
			Window: time.Hour}},
		{"window below the minimum", SageActions, Args{Window: MinWindow - time.Second}},
		{"window above the maximum", SageActions, Args{Window: MaxWindow + time.Second}},
		{"pid on a window probe", SageActions, Args{PID: 5}},
	}
	for _, c := range bad {
		err := reg.CheckArgs(c.id, c.args)
		if !errors.Is(err, ErrInvalidArgs) {
			t.Errorf("%s: CheckArgs = %v, want ErrInvalidArgs", c.name, err)
		}
		if errors.Is(err, ErrUnknownProbe) {
			t.Errorf("%s: a known probe was reported unknown", c.name)
		}
	}
}

func TestCheckArgs_UnknownProbe(t *testing.T) {
	for _, id := range []ID{"", "drop_table", "LOCK_GRAPH", "lock_graph; select 1"} {
		err := Catalog().CheckArgs(id, Args{})
		if !errors.Is(err, ErrUnknownProbe) || errors.Is(err, ErrInvalidArgs) {
			t.Errorf("CheckArgs(%q) = %v, want only ErrUnknownProbe", id, err)
		}
	}
	var nilReg *Registry
	if err := nilReg.CheckArgs(LockGraph, Args{}); !errors.Is(err, ErrUnknownProbe) {
		t.Errorf("nil registry = %v, want ErrUnknownProbe", err)
	}
}
