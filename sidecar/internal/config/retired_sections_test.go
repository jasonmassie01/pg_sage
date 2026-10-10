package config

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// A retired section is a top-level key that an older release understood and
// this one does not. A package that owns its removal registers it: the loader
// hands the section to its Check and drops it before the strict decode, so a
// config that still carries it loads (or is refused by Check, by name).

func registerTestSection(t *testing.T, s RetiredSection) {
	t.Helper()
	restore := replaceRetiredSectionsForTest()
	t.Cleanup(restore)
	RegisterRetiredSection(s)
}

func captureConfigWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := configWarningOutput
	configWarningOutput = &out
	t.Cleanup(func() { configWarningOutput = previous })
	return &out
}

func TestRetiredSection_IsDroppedAndItsWarningsPrinted(t *testing.T) {
	var seen []string
	registerTestSection(t, RetiredSection{Key: "legacy_demo",
		Check: func(node *yaml.Node) ([]string, error) {
			seen = append(seen, node.Content[0].Value)
			return []string{"WARNING: legacy_demo is ignored"}, nil
		}})
	out := captureConfigWarnings(t)
	cfg := DefaultConfig()
	yml := "legacy_demo:\n  enabled: true\ncollector:\n  interval_seconds: 17\n"
	if err := loadYAML(writeYAMLFile(t, yml), cfg); err != nil {
		t.Fatalf("loadYAML: %v", err)
	}
	if cfg.Collector.IntervalSeconds != 17 {
		t.Fatalf("collector.interval_seconds = %d, want 17 (rest of the file "+
			"must still load)", cfg.Collector.IntervalSeconds)
	}
	if len(seen) != 1 || seen[0] != "enabled" {
		t.Fatalf("Check saw %v, want the section's own mapping", seen)
	}
	if got := out.String(); !strings.Contains(got, "WARNING: legacy_demo is ignored") {
		t.Fatalf("warnings = %q", got)
	}
}

func TestRetiredSection_CheckErrorRefusesTheConfig(t *testing.T) {
	refuse := errors.New("legacy_demo.live is set: see the runbook")
	registerTestSection(t, RetiredSection{Key: "legacy_demo",
		Check: func(*yaml.Node) ([]string, error) { return nil, refuse }})
	cfg := DefaultConfig()
	before := Clone(cfg)
	err := loadYAML(writeYAMLFile(t, "legacy_demo:\n  live: true\n"), cfg)
	if !errors.Is(err, refuse) {
		t.Fatalf("loadYAML error = %v, want the Check error", err)
	}
	if !strings.Contains(err.Error(), "legacy_demo") {
		t.Fatalf("error %q does not name the section", err)
	}
	if cfg.Collector != before.Collector {
		t.Fatal("a refused config mutated the candidate")
	}
}

func TestRetiredSection_AbsentSectionNeverCallsCheck(t *testing.T) {
	called := false
	registerTestSection(t, RetiredSection{Key: "legacy_demo",
		Check: func(*yaml.Node) ([]string, error) { called = true; return nil, nil }})
	cfg := DefaultConfig()
	if err := loadYAML(writeYAMLFile(t, "collector:\n  interval_seconds: 9\n"), cfg); err != nil {
		t.Fatalf("loadYAML: %v", err)
	}
	if called {
		t.Fatal("Check ran for a section the file does not carry")
	}
	if cfg.Collector.IntervalSeconds != 9 {
		t.Fatalf("interval = %d, want 9", cfg.Collector.IntervalSeconds)
	}
}

func TestRetiredSection_UnregisteredKeyStaysUnknown(t *testing.T) {
	restore := replaceRetiredSectionsForTest()
	t.Cleanup(restore)
	err := loadYAML(writeYAMLFile(t, "legacy_demo:\n  enabled: true\n"), DefaultConfig())
	if err == nil || !strings.Contains(err.Error(), "legacy_demo") {
		t.Fatalf("an unregistered key must stay a strict-decode error, got %v", err)
	}
}

func TestRetiredSection_NullAndScalarSectionsReachCheck(t *testing.T) {
	var kinds []yaml.Kind
	registerTestSection(t, RetiredSection{Key: "legacy_demo",
		Check: func(node *yaml.Node) ([]string, error) {
			kinds = append(kinds, node.Kind)
			return nil, nil
		}})
	for _, yml := range []string{"legacy_demo:\n", "legacy_demo: true\n"} {
		if err := loadYAML(writeYAMLFile(t, yml), DefaultConfig()); err != nil {
			t.Fatalf("loadYAML(%q): %v", yml, err)
		}
	}
	if len(kinds) != 2 || kinds[0] != yaml.ScalarNode || kinds[1] != yaml.ScalarNode {
		t.Fatalf("Check saw kinds %v, want two scalar nodes", kinds)
	}
}

func TestRegisterRetiredSection_RejectsInvalidRegistrations(t *testing.T) {
	restore := replaceRetiredSectionsForTest()
	t.Cleanup(restore)
	check := func(*yaml.Node) ([]string, error) { return nil, nil }
	for name, s := range map[string]RetiredSection{
		"empty key":     {Key: " ", Check: check},
		"nil check":     {Key: "legacy_demo"},
		"live key":      {Key: "collector", Check: check},
		"registered 2x": {Key: "legacy_twice", Check: check},
	} {
		if name == "registered 2x" {
			RegisterRetiredSection(s)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: RegisterRetiredSection did not panic", name)
				}
			}()
			RegisterRetiredSection(s)
		}()
	}
}

// Registration happens from package init functions, but reloads read the
// registry from other goroutines; both must be safe together.
func TestRetiredSection_ConcurrentLoadsAndRegistration(t *testing.T) {
	registerTestSection(t, RetiredSection{Key: "legacy_demo",
		Check: func(*yaml.Node) ([]string, error) { return nil, nil }})
	captureConfigWarnings(t)
	path := writeYAMLFile(t, "legacy_demo: {}\n")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i == 0 {
				RegisterRetiredSection(RetiredSection{Key: "legacy_other",
					Check: func(*yaml.Node) ([]string, error) { return nil, nil }})
			}
			errs <- loadYAML(path, DefaultConfig())
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent load: %v", err)
		}
	}
}
