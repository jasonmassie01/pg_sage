package selfconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// The rule registry: every rule names a derivable key with known
// lifecycle, reads and writes it, and is documented. Adding a key is one
// Rule value; these checks keep that cheap and safe.

func TestRulesAreWellFormed(t *testing.T) {
	rules := Rules()
	if len(rules) != 5 {
		t.Fatalf("%d rules, want the 5 initial derived keys", len(rules))
	}
	if err := ValidateRules(rules); err != nil {
		t.Fatalf("registry invalid: %v", err)
	}
	seen := map[string]bool{}
	def := config.DefaultConfig()
	for _, r := range rules {
		if seen[r.Key] {
			t.Errorf("%s registered twice", r.Key)
		}
		seen[r.Key] = true
		if class, _ := config.KeyClassOf(r.Key); class != config.KeyDerivable {
			t.Errorf("%s is classified %q, not derivable", r.Key, class)
		}
		if _, ok := config.LookupFieldLifecycle(r.Key); !ok {
			t.Errorf("%s has no lifecycle", r.Key)
		}
		if r.Name == "" || r.Version < 1 || r.Summary == "" || r.Unit == "" ||
			r.EvidenceDoc == "" || r.Step <= 0 || r.HardMin > r.HardMax {
			t.Errorf("%s incomplete: %+v", r.Key, r)
		}
		d := r.Get(def)
		if d < r.HardMin || d > r.HardMax {
			t.Errorf("%s default %v outside [%v, %v]", r.Key, d, r.HardMin, r.HardMax)
		}
		c := config.Clone(def)
		r.Set(c, r.HardMax)
		if r.Get(c) != r.HardMax {
			t.Errorf("%s Set/Get round trip: %v", r.Key, r.Get(c))
		}
		if got, ok := Lookup(r.Key); !ok || got.Name != r.Name {
			t.Errorf("Lookup(%s) = %v %v", r.Key, got.Name, ok)
		}
	}
	if _, ok := Lookup("trust.level"); ok {
		t.Fatal("Lookup found a rule for trust.level")
	}
}

func TestRulesReturnsACopy(t *testing.T) {
	rules := Rules()
	rules[0].Key = "trust.level"
	if Rules()[0].Key == "trust.level" {
		t.Fatal("mutating Rules() changed the registry")
	}
}

func TestValidateRulesRefusesUnsafeOrBrokenRules(t *testing.T) {
	good, _ := Lookup("collector.interval_seconds")
	mutate := map[string]func(*Rule){
		"safety-critical key":  func(r *Rule) { r.Key = "trust.ramp_safe_hours" },
		"preference key":       func(r *Rule) { r.Key = "alerting.cooldown_minutes" },
		"unknown key":          func(r *Rule) { r.Key = "collector.nope" },
		"no name":              func(r *Rule) { r.Name = "" },
		"version zero":         func(r *Rule) { r.Version = 0 },
		"no direction":         func(r *Rule) { r.Widens = 0 },
		"inverted hard range":  func(r *Rule) { r.HardMin, r.HardMax = 10, 1 },
		"no step":              func(r *Rule) { r.Step = 0 },
		"no derive":            func(r *Rule) { r.Derive = nil },
		"no getter":            func(r *Rule) { r.Get = nil },
		"no setter":            func(r *Rule) { r.Set = nil },
		"fixed cap, no reason": func(r *Rule) { r.Cap, r.CapNote = CapFixed, "" },
	}
	for name, m := range mutate {
		r := good
		m(&r)
		if err := ValidateRules([]Rule{r}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateRules([]Rule{good, good}); err == nil ||
		!strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate key: %v", err)
	}
}

func TestDerivedSettingsReferenceIsGenerated(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "generated",
		"derived-settings.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if strings.ReplaceAll(string(got), "\r\n", "\n") != Markdown() {
		t.Fatal("docs/generated/derived-settings.md drifted; regenerate with " +
			"`go run ./cmd/gen_config_meta -derived-out ../docs/generated/derived-settings.md`")
	}
	for _, r := range Rules() {
		if !strings.Contains(Markdown(), "`"+r.Key+"`") {
			t.Errorf("reference omits %s", r.Key)
		}
	}
}

func TestMarkdownDescribesBoundsAndShadow(t *testing.T) {
	md := Markdown()
	for _, want := range []string{"# Derived settings", "shadow", "pinned", "soak",
		"never", "restart", "| `safety.query_timeout_ms` |", "500-5000"} {
		if !strings.Contains(md, want) {
			t.Errorf("reference omits %q", want)
		}
	}
}
