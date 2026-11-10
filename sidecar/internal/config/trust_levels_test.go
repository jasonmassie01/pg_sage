package config

import (
	"sync"
	"testing"
)

// config-10: one trust enum shared by the store, the API and cmd.

func TestTrustLevels_OrderedLeastToMostAutonomous(t *testing.T) {
	want := []string{"observation", "advisory", "autonomous"}
	got := TrustLevels()
	if len(got) != len(want) {
		t.Fatalf("TrustLevels() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TrustLevels()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestTrustLevels_ReturnsCopy(t *testing.T) {
	first := TrustLevels()
	first[0] = "autonomous"
	if got := TrustLevels()[0]; got != "observation" {
		t.Fatalf("mutating the returned slice changed the registry: %q", got)
	}
}

func TestTrustRank_Boundaries(t *testing.T) {
	cases := map[string]int{
		"observation": 0,
		"advisory":    1,
		"autonomous":  2,
		"":            -1,
		"Autonomous":  -1,
		"auto":        -1,
		" advisory":   -1,
		"observe":     -1,
	}
	for level, want := range cases {
		if got := TrustRank(level); got != want {
			t.Errorf("TrustRank(%q) = %d, want %d", level, got, want)
		}
	}
}

func TestIsTrustLevel_ExactlyTheRegistry(t *testing.T) {
	for _, level := range TrustLevels() {
		if !IsTrustLevel(level) {
			t.Errorf("IsTrustLevel(%q) = false for a registry level", level)
		}
	}
	for _, level := range []string{"", "ADVISORY", "manual", "approval", "2"} {
		if IsTrustLevel(level) {
			t.Errorf("IsTrustLevel(%q) = true, want false", level)
		}
	}
}

func TestTrustRank_DefaultIsLowest(t *testing.T) {
	if TrustRank(DefaultTrustLevel) != 0 {
		t.Fatalf("default trust %q has rank %d, want 0 (least autonomy)",
			DefaultTrustLevel, TrustRank(DefaultTrustLevel))
	}
}

func TestTrustLevels_ConcurrentReaders(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			levels := TrustLevels()
			levels[1] = "mutated"
			if TrustRank("advisory") != 1 {
				t.Error("concurrent copy mutation changed TrustRank")
			}
		}()
	}
	wg.Wait()
}

func TestValidateTrust_AcceptsExactlyTrustLevels(t *testing.T) {
	for _, level := range TrustLevels() {
		cfg := DefaultConfig()
		cfg.Trust.Level = level
		if err := cfg.validateTrust(); err != nil {
			t.Errorf("validateTrust(%q) = %v, want nil", level, err)
		}
	}
	for _, level := range []string{"", "Autonomous", "auto", "observe"} {
		cfg := DefaultConfig()
		cfg.Trust.Level = level
		if err := cfg.validateTrust(); err == nil {
			t.Errorf("validateTrust(%q) accepted a level outside TrustLevels", level)
		}
	}
}
