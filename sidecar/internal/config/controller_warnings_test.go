package config

import (
	"context"
	"reflect"
	"testing"
)

type warningOwner struct{ warnings []string }

func (warningOwner) Name() string { return "trust_policy" }

func (o warningOwner) Prepare(
	context.Context, ConfigSnapshot, ConfigSnapshot,
) (PreparedReconfiguration, error) {
	return warningPrepared{warnings: o.warnings}, nil
}

type warningPrepared struct{ warnings []string }

func (warningPrepared) Commit(context.Context) error   { return nil }
func (warningPrepared) Rollback(context.Context) error { return nil }
func (warningPrepared) Drain(context.Context) error    { return nil }
func (p warningPrepared) Warnings() []string           { return p.warnings }

// G5-B14: an owner that applied a change only partially (e.g. a global trust
// raise that must not escalate per-database policy) reports it to the API.
func TestApplySurfacesOwnerWarningsAfterCommit(t *testing.T) {
	want := []string{"2 databases keep their own trust level"}
	controller := NewConfigController(DefaultConfig(), nil,
		warningOwner{warnings: want})
	candidate := DefaultConfig()
	candidate.Trust.Level = "advisory"

	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !reflect.DeepEqual(result.Warnings, want) {
		t.Fatalf("warnings = %v, want %v", result.Warnings, want)
	}
	if result.ComponentStatus["trust_policy"] != "applied" {
		t.Fatalf("status = %+v, want applied", result.ComponentStatus)
	}
}

func TestApplyWithoutOwnerWarningsKeepsWarningsEmpty(t *testing.T) {
	controller := NewConfigController(DefaultConfig(), nil, warningOwner{})
	candidate := DefaultConfig()
	candidate.Trust.Level = "advisory"
	result, err := controller.Apply(context.Background(), 1, candidate)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", result.Warnings)
	}
}
