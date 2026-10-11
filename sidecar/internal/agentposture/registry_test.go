package agentposture

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeDetector is a detector whose spec and output a test sets.
type fakeDetector struct {
	spec     Spec
	findings []Finding
	err      error
	seen     *Input
}

func (f *fakeDetector) Spec() Spec { return f.spec }

func (f *fakeDetector) Detect(_ context.Context, in Input) ([]Finding, error) {
	if f.seen != nil {
		*f.seen = in
	}
	return f.findings, f.err
}

func fake(id string, sev Severity, arms ...Arm) *fakeDetector {
	return &fakeDetector{spec: Spec{ID: id, Title: "fake " + id, Severity: sev, Arms: arms}}
}

func TestRegistry_RegisterAndListSortedByID(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"AP-10", "AP-02", "AP-01"} {
		if err := r.Register(fake(id, Warning)); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	var ids []string
	for _, d := range r.Detectors() {
		ids = append(ids, d.Spec().ID)
	}
	if got := strings.Join(ids, ","); got != "AP-01,AP-02,AP-10" {
		t.Fatalf("detectors = %s, want AP-01,AP-02,AP-10", got)
	}
	d, ok := r.Get("AP-02")
	if !ok || d.Spec().ID != "AP-02" {
		t.Fatalf("Get(AP-02) = %v, %v", d, ok)
	}
	if _, ok := r.Get("AP-99"); ok {
		t.Fatal("Get(AP-99) found a detector that was never registered")
	}
}

func TestRegistry_RejectsInvalidSpecs(t *testing.T) {
	cases := []struct {
		name string
		d    Detector
	}{
		{"nil detector", nil},
		{"empty id", fake("", Warning)},
		{"lower-case id", fake("ap-01", Warning)},
		{"one digit", fake("AP-1", Warning)},
		{"three digits", fake("AP-001", Warning)},
		{"other prefix", fake("XP-01", Warning)},
		{"no title", &fakeDetector{spec: Spec{ID: "AP-03", Severity: Warning}}},
		{"empty severity", fake("AP-04", "")},
		{"unknown severity", fake("AP-05", "fatal")},
		{"arm without name", fake("AP-06", Warning, Arm{MinVersion: 150000})},
		{"arm range empty", fake("AP-07", Warning,
			Arm{Name: "x", MinVersion: 150000, MaxVersion: 150000, SkipReason: "r"})},
		{"arm range inverted", fake("AP-08", Warning,
			Arm{Name: "x", MinVersion: 160000, MaxVersion: 140000, SkipReason: "r"})},
		{"arm without skip reason", fake("AP-09", Warning, Arm{Name: "x", MinVersion: 150000})},
		{"duplicate arm", fake("AP-11", Warning,
			Arm{Name: "x", MinVersion: 150000, SkipReason: "r"},
			Arm{Name: "x", MaxVersion: 150000, SkipReason: "r"})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewRegistry()
			err := r.Register(c.d)
			if !errors.Is(err, ErrInvalidDetector) {
				t.Fatalf("Register = %v, want ErrInvalidDetector", err)
			}
			if n := len(r.Detectors()); n != 0 {
				t.Fatalf("a rejected detector was kept (%d registered)", n)
			}
		})
	}
}

func TestRegistry_RejectsDuplicateID(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(fake("AP-01", Critical)); err != nil {
		t.Fatalf("first register: %v", err)
	}
	err := r.Register(fake("AP-01", Warning))
	if !errors.Is(err, ErrInvalidDetector) || !strings.Contains(err.Error(), "AP-01") {
		t.Fatalf("duplicate register = %v, want ErrInvalidDetector naming AP-01", err)
	}
	d, _ := r.Get("AP-01")
	if d.Spec().Severity != Critical {
		t.Fatal("the duplicate replaced the first registration")
	}
}

func TestRegister_PanicsOnInvalidDetector(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Register of an invalid detector into the default registry did not panic")
		}
	}()
	Register(fake("bad", Warning))
}

func TestRegistry_NilRegistryIsEmpty(t *testing.T) {
	var r *Registry
	if n := len(r.Detectors()); n != 0 {
		t.Fatalf("nil registry lists %d detectors", n)
	}
	if _, ok := r.Get("AP-01"); ok {
		t.Fatal("nil registry found a detector")
	}
	if err := r.Register(fake("AP-01", Info)); !errors.Is(err, ErrInvalidDetector) {
		t.Fatalf("register into nil registry = %v", err)
	}
}

func TestRegistry_DetectorsIsACopy(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(fake("AP-01", Info))
	list := r.Detectors()
	list[0] = fake("AP-99", Info)
	if d := r.Detectors()[0]; d.Spec().ID != "AP-01" {
		t.Fatalf("mutating the returned slice changed the registry: %s", d.Spec().ID)
	}
}

// Concurrent access: detectors register from init functions, but the
// registry is also read by every database runtime at once.
func TestRegistry_ConcurrentRegisterAndRead(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 1; i <= 40; i++ {
		wg.Add(2)
		id := "AP-" + twoDigits(i)
		go func() {
			defer wg.Done()
			if err := r.Register(fake(id, Info)); err != nil {
				t.Errorf("register %s: %v", id, err)
			}
		}()
		go func() {
			defer wg.Done()
			_ = r.Detectors()
			_, _ = r.Get(id)
		}()
	}
	wg.Wait()
	if n := len(r.Detectors()); n != 40 {
		t.Fatalf("registered %d detectors, want 40", n)
	}
}

func twoDigits(i int) string {
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func TestArm_Applies(t *testing.T) {
	cases := []struct {
		arm     Arm
		version int
		want    bool
	}{
		{Arm{Name: "pg15+", MinVersion: 150000}, 149999, false},
		{Arm{Name: "pg15+", MinVersion: 150000}, 150000, true},
		{Arm{Name: "pg15+", MinVersion: 150000}, 180001, true},
		{Arm{Name: "pg14", MaxVersion: 150000}, 140012, true},
		{Arm{Name: "pg14", MaxVersion: 150000}, 150000, false},
		{Arm{Name: "pg14-15", MinVersion: 140000, MaxVersion: 160000}, 159999, true},
		{Arm{Name: "pg14-15", MinVersion: 140000, MaxVersion: 160000}, 160000, false},
		{Arm{Name: "all"}, 140000, true},
		// An unknown server version (0) satisfies no bounded arm.
		{Arm{Name: "pg15+", MinVersion: 150000}, 0, false},
		{Arm{Name: "pg14", MaxVersion: 150000}, 0, false},
	}
	for _, c := range cases {
		if got := c.arm.Applies(c.version); got != c.want {
			t.Errorf("%s.Applies(%d) = %v, want %v", c.arm.Name, c.version, got, c.want)
		}
	}
}

func TestSeverity_ValidAndAtMost(t *testing.T) {
	for _, s := range []Severity{Info, Warning, Critical} {
		if !s.Valid() {
			t.Errorf("%q not valid", s)
		}
	}
	for _, s := range []Severity{"", "high", "CRITICAL"} {
		if s.Valid() {
			t.Errorf("%q valid", s)
		}
	}
	if !Info.AtMost(Warning) || !Warning.AtMost(Warning) || Critical.AtMost(Warning) {
		t.Fatal("AtMost does not order info < warning < critical")
	}
	if Severity("bogus").AtMost(Critical) {
		t.Fatal("an unknown severity must not be at most critical")
	}
}
