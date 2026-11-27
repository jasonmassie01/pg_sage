package metricsreg

import (
	"sort"
	"strings"
	"sync"
	"testing"
)

// observability-09: the registry's own rules. The scrape contract (every
// emitted family registered, every registered family emitted) lives in
// cmd/pg_sage_sidecar/metrics_registry_test.go.

func validFamily() Family {
	return Family{Name: "pg_sage_example_up", Type: Gauge, Area: AreaProcess,
		Help: "1 when the example is up", Labels: []string{"database"},
		Modes: AllModes, Since: "v2.6.0"}
}

// Happy path: the shipped registry satisfies every rule.
func TestRegistryIsValid(t *testing.T) {
	fams := Families()
	if len(fams) < 90 {
		t.Fatalf("registry has %d families; the scrape exports over 90", len(fams))
	}
	if err := Validate(fams, RetiredFamilies()); err != nil {
		t.Fatalf("registry is invalid:\n%v", err)
	}
}

// Invalid input: each broken entry fails with a message naming the rule.
func TestValidateRejectsBrokenEntries(t *testing.T) {
	cases := map[string]struct {
		edit func(*Family)
		want string
	}{
		"gauge named _total": {func(f *Family) { f.Name = "pg_sage_example_total" },
			"a gauge must not end in _total"},
		"counter without _total": {func(f *Family) { f.Type = Counter },
			"a counter must end in _total"},
		"summary named _count": {func(f *Family) {
			f.Type, f.Name = Summary, "pg_sage_wait_seconds_count"
			f.Unit = "seconds"
		}, "a summary must not end in _total, _sum, _count or _bucket"},
		"unknown type":   {func(f *Family) { f.Type = "untyped" }, `type "untyped"`},
		"empty help":     {func(f *Family) { f.Help = "" }, "help must be one non-empty line"},
		"multiline help": {func(f *Family) { f.Help = "a\nb" }, "help must be one"},
		"backslash help": {func(f *Family) { f.Help = `a\b` }, "without backslashes"},
		"foreign prefix": {func(f *Family) { f.Name = "sage_example_up" },
			"name must be pg_sage_ or process_"},
		"upper case":   {func(f *Family) { f.Name = "pg_sage_Example_up" }, "name must be"},
		"unknown area": {func(f *Family) { f.Area = "misc" }, `area "misc"`},
		"bad since":    {func(f *Family) { f.Since = "2.7" }, `since "2.7"`},
		"seconds without unit": {func(f *Family) { f.Name = "pg_sage_example_seconds" },
			`must declare unit "seconds"`},
		"bytes with wrong unit": {func(f *Family) {
			f.Name, f.Unit = "pg_sage_example_bytes", "seconds"
		}, `must declare unit "bytes", not "seconds"`},
		"bad label":      {func(f *Family) { f.Labels = []string{"Database"} }, `label "Database"`},
		"repeated label": {func(f *Family) { f.Labels = []string{"a", "a"} }, `label "a"`},
		"unknown mode":   {func(f *Family) { f.Modes = []string{"cluster"} }, `mode "cluster"`},
		"no mode":        {func(f *Family) { f.Modes = nil }, "no mode renders the family"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := validFamily()
			c.edit(&f)
			err := Validate([]Family{f}, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate(%+v) = %v, want an error containing %q", f, err, c.want)
			}
			if !strings.Contains(err.Error(), f.Name) {
				t.Errorf("error %q does not name the family %s", err, f.Name)
			}
		})
	}
}

// Boundary: the one pinned legacy gauge may keep _total; no other may.
func TestValidateAllowsOnlyThePinnedLegacyTotalGauge(t *testing.T) {
	f := validFamily()
	f.Name = "pg_sage_incidents_avoided_total"
	if err := Validate([]Family{f}, nil); err != nil {
		t.Fatalf("pinned legacy gauge refused: %v", err)
	}
	f.Name = "pg_sage_incidents_prevented_total"
	if err := Validate([]Family{f}, nil); err == nil {
		t.Fatal("a new _total gauge passed validation")
	}
	if err := Validate([]Family{validFamily()}, nil); err != nil {
		t.Fatalf("valid family refused: %v", err)
	}
}

func TestValidateRejectsDuplicatesAndBrokenRetirements(t *testing.T) {
	f := validFamily()
	cases := map[string]struct {
		gone []Retired
		fams []Family
		want string
	}{
		"duplicate": {nil, []Family{f, f}, "registered twice"},
		"retired but registered": {[]Retired{{Name: f.Name, Note: "x", Release: "v2.6.0"}},
			[]Family{f}, "retired in v2.6.0 but still registered"},
		"unregistered replacement": {[]Retired{{Name: "pg_sage_old",
			Replacement: "pg_sage_new", Release: "v2.6.0"}}, nil,
			"replacement pg_sage_new is not registered"},
		"no replacement or note": {[]Retired{{Name: "pg_sage_old", Release: "v2.6.0"}},
			nil, "neither a replacement nor a note"},
		"bad release": {[]Retired{{Name: "pg_sage_old", Note: "x", Release: "soon"}}, nil,
			`release "soon"`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := Validate(c.fams, c.gone)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// Nil/empty: nothing to check is valid; an empty family breaks every rule.
func TestValidateEmptyInputs(t *testing.T) {
	if err := Validate(nil, nil); err != nil {
		t.Fatalf("Validate(nil, nil) = %v, want nil", err)
	}
	err := Validate([]Family{{}}, nil)
	if err == nil {
		t.Fatal("zero Family passed validation")
	}
	for _, want := range []string{"name must be", "help must be", "no mode renders"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("zero Family error lacks %q:\n%v", want, err)
		}
	}
}

// The v2.6.0 rename release: exactly the removal and rename set the plan
// names, each with a registered replacement or a note.
func TestRetiredListsTheRenameRelease(t *testing.T) {
	want := map[string]string{
		"pg_sage_findings_total": "pg_sage_findings_open",
		"pg_sage_collector_last_run_timestamp": "pg_sage_collector_" +
			"last_success_timestamp_seconds",
		"pg_sage_fleet_instance_findings":         "pg_sage_findings_open",
		"pg_sage_fleet_instance_health":           "pg_sage_database_health_score",
		"pg_sage_connections_total":               "",
		"pg_sage_cache_hit_ratio":                 "",
		"pg_sage_optimizer_recommendations_total": "pg_sage_optimizer_recommendations",
		"pg_sage_fleet_findings_total":            "pg_sage_fleet_findings_open",
	}
	got := RetiredFamilies()
	if len(got) != len(want) {
		t.Fatalf("retired families = %d, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		repl, ok := want[r.Name]
		if !ok || repl != r.Replacement || r.Release != "v2.6.0" {
			t.Errorf("retired %+v, want replacement %q in v2.6.0 (listed %v)", r, repl, ok)
		}
		if _, still := Lookup(r.Name); still {
			t.Errorf("retired %s is still registered", r.Name)
		}
	}
}

func TestHeaderWritesTheRegisteredHelpAndType(t *testing.T) {
	var b strings.Builder
	Header(&b, "pg_sage_database_up")
	Header(&b, "pg_sage_actions_total")
	want := "# HELP pg_sage_database_up 1 if the monitored database is connected and " +
		"its last status refresh succeeded\n# TYPE pg_sage_database_up gauge\n" +
		"# HELP pg_sage_actions_total "
	if !strings.HasPrefix(b.String(), want) ||
		!strings.HasSuffix(b.String(), "\n# TYPE pg_sage_actions_total counter\n") {
		t.Fatalf("Header wrote:\n%s", b.String())
	}
}

// Error propagation: an unregistered name panics under test and, in a
// running sidecar, still leaves a parseable TYPE line.
func TestHeaderUnregisteredName(t *testing.T) {
	func() {
		defer func() {
			r := recover()
			msg, _ := r.(string)
			if !strings.Contains(msg, `"pg_sage_nope" is not registered`) {
				t.Fatalf("recover() = %v, want a panic naming pg_sage_nope", r)
			}
		}()
		var b strings.Builder
		Header(&b, "pg_sage_nope")
	}()
	if !strict() {
		t.Fatal("strict() is off under go test")
	}
	off := false
	strictOverride = &off
	t.Cleanup(func() { strictOverride = nil })
	var b strings.Builder
	Header(&b, "pg_sage_nope")
	if b.String() != "# TYPE pg_sage_nope untyped\n" {
		t.Fatalf("non-strict Header = %q, want an untyped TYPE line only", b.String())
	}
}

func TestFamiliesIsACopyGroupedByArea(t *testing.T) {
	fams := Families()
	fams[0].Labels = append(fams[0].Labels, "mutated")
	fams[0].Modes[0] = "mutated"
	f, ok := Lookup(fams[0].Name)
	if !ok || strings.Contains(strings.Join(f.Labels, ","), "mutated") ||
		f.Modes[0] == "mutated" {
		t.Fatalf("Families shares state with the registry: %+v", f)
	}
	if _, ok := Lookup("pg_sage_nope"); ok {
		t.Fatal("Lookup found an unregistered name")
	}
	order := map[string]int{}
	for i, a := range Areas() {
		order[a.ID] = i
	}
	if !sort.SliceIsSorted(fams, func(i, j int) bool {
		return order[fams[i].Area] < order[fams[j].Area]
	}) {
		t.Fatal("Families is not grouped by area in Areas order")
	}
	for _, a := range Areas() {
		if a.Title == "" || a.Intro == "" {
			t.Errorf("area %s lacks a title or intro", a.ID)
		}
	}
}

// Concurrent access: scrapes call Header from many goroutines; the
// registry is read-only after init.
func TestHeaderConcurrentCallsAgree(t *testing.T) {
	var wg sync.WaitGroup
	out := make([]string, 16)
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var b strings.Builder
			for _, f := range Families() {
				Header(&b, f.Name)
			}
			out[i] = b.String()
		}(i)
	}
	wg.Wait()
	for i := range out {
		if out[i] != out[0] || out[i] == "" {
			t.Fatalf("goroutine %d rendered a different header set", i)
		}
	}
}
