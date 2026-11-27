package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/specialist"
)

// The specialist limits are carried from configuration into the contract
// unchanged, including the live-investigation age cap.

// No concurrent access tests: specialistLimits is a pure conversion.

func TestSpecialistLimits_CarriesEveryConfiguredBound(t *testing.T) {
	sc := config.DefaultConfig().Specialist
	sc.WritesPerMinute, sc.ReadsPerMinute = 7, 70
	sc.MaxOpenPerIdentity, sc.MaxOpenTotal, sc.MaxLiveAgeHours = 2, 5, 6
	got := specialistLimits(sc)
	want := specialist.Limits{WritesPerMinute: 7, ReadsPerMinute: 70,
		MaxOpenPerIdentity: 2, MaxOpenTotal: 5, MaxLiveAge: 6 * time.Hour}
	if got != want {
		t.Fatalf("limits %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("configured limits refused: %v", err)
	}
}

func TestSpecialistLimits_DefaultsMatchTheContractDefaults(t *testing.T) {
	got := specialistLimits(config.DefaultConfig().Specialist)
	if want := specialist.DefaultLimits(); got != want {
		t.Fatalf("configuration defaults %+v differ from the contract defaults %+v", got, want)
	}
}

func TestBuildSpecialist_WiresTheAgeCap(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := fleet.NewManager(cfg)
	if rt, err := buildSpecialistWithStore(cfg, mgr, newSpecialistMemStore(), nil,
		nil); err != nil || rt == nil {
		t.Fatalf("default limits: %v", err)
	}
	cfg.Specialist.MaxLiveAgeHours = 0 // bypasses config validation: the contract refuses it
	_, err := buildSpecialistWithStore(cfg, mgr, newSpecialistMemStore(), nil, nil)
	if !errors.Is(err, specialist.ErrInvalid) || !strings.Contains(err.Error(),
		"live investigation") {
		t.Fatalf("a zero live-investigation age cap: %v", err)
	}
}
