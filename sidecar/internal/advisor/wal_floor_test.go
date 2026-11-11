package advisor

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// advisors-05: the max_wal_size floor is deterministic; the model narrates.

// No concurrent access tests: the filter is a pure function of its inputs.

func walModelAnswer(value string) string {
	return `[{"object_identifier":"instance","severity":"warning",` +
		`"rationale":"checkpoints are forced",` +
		`"recommended_sql":"ALTER SYSTEM SET max_wal_size = '` + value + `'"}]`
}

// maxWALTarget is the bytes a finding's max_wal_size statement sets.
func maxWALTarget(t *testing.T, f analyzer.Finding) (float64, bool) {
	t.Helper()
	st, ok := pgconf.ParseAlterSystem(f.RecommendedSQL)
	if !ok || st.Name != "max_wal_size" {
		return 0, false
	}
	v, err := pgconf.ParseValue(st.Value, pgconf.Docs["max_wal_size"])
	if err != nil {
		t.Fatalf("value %q: %v", st.Value, err)
	}
	return v, true
}

// Boundary: a model value of floor - 1 MB is dropped and the floor finding,
// with its arithmetic, is emitted instead; the floor itself is kept.
func TestWALFloor_ModelBelowFloorIsReplaced(t *testing.T) {
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	_, got, err := walWindowAdvice(t, walHistory1h(), snap, walModelAnswer("1535MB"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("findings = %+v, want only the floor finding", got)
	}
	f := got[0]
	if v, ok := maxWALTarget(t, f); !ok || v != 1536<<20 {
		t.Fatalf("SQL = %q, want max_wal_size = '1536MB'", f.RecommendedSQL)
	}
	if f.Category != "wal_tuning" || f.ObjectIdentifier != "instance" ||
		f.ActionRisk == "" {
		t.Errorf("floor finding = %+v, want an executable wal_tuning instance row", f)
	}
	for key, want := range map[string]any{"max_wal_size_floor_bytes": float64(1536 << 20),
		"wal_per_checkpoint_timeout_bytes": float64(1 << 30),
		"wal_bytes_per_hour":               float64(12 << 30),
		"window_seconds":                   float64(3600), "requested_share": 0.9375,
		"current_max_wal_size_bytes": float64(1024 << 20)} {
		if f.Detail[key] != want {
			t.Errorf("detail[%s] = %v, want %v", key, f.Detail[key], want)
		}
	}
	if math, _ := f.Detail["arithmetic"].(string); !strings.Contains(math, "1.5 x") {
		t.Errorf("arithmetic = %q, want the floor's formula", math)
	}
	_, got, _ = walWindowAdvice(t, walHistory1h(), snap, walModelAnswer("1536MB"))
	if len(got) != 1 || got[0].Detail["max_wal_size_floor_bytes"] != nil {
		t.Errorf("a model value at the floor was not kept as is: %+v", got)
	}
}

// Rule 9 is code: a value below the current one is dropped even when the
// floor is lower; no floor finding is emitted when the floor would not be a
// raise either.
func TestWALFloor_BelowCurrentIsDropped(t *testing.T) {
	snap := walWindowSnap("4096", "300", walCounters(130, 52, 22<<30))
	_, got, err := walWindowAdvice(t, walHistory1h(), snap, walModelAnswer("2GB"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("findings = %+v, want the reduction dropped and no floor finding", got)
	}
}

// The model may raise up to 4 x floor; above that the value is advice only.
func TestWALFloor_AboveCeilingIsAdvisory(t *testing.T) {
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	_, got, _ := walWindowAdvice(t, walHistory1h(), snap, walModelAnswer("6144MB"))
	if len(got) != 1 || got[0].RecommendedSQL == "" {
		t.Fatalf("4 x floor: findings = %+v, want it executable", got)
	}
	_, got, _ = walWindowAdvice(t, walHistory1h(), snap, walModelAnswer("6145MB"))
	if len(got) != 1 || got[0].RecommendedSQL != "" ||
		!strings.Contains(got[0].Recommendation, "4 x") {
		t.Fatalf("above 4 x floor: findings = %+v, want advice only", got)
	}
}

// Nil/empty: with no history row the WAL advisor is advisory: the model is
// asked, but nothing it says is executable.
func TestWALFloor_NoHistoryStaysAdvisory(t *testing.T) {
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	answer := `[{"object_identifier":"instance","rationale":"x","recommended_sql":` +
		`"ALTER SYSTEM SET max_wal_size = '4GB'; ` +
		`ALTER SYSTEM SET checkpoint_completion_target = 0.9"}]`
	prompts, got, err := walWindowAdvice(t, &fakeWindowHistory{}, snap, answer)
	if err != nil || len(prompts) != 1 {
		t.Fatalf("prompts=%d err=%v, want the model asked", len(prompts), err)
	}
	if len(got) != 2 {
		t.Fatalf("findings = %+v, want both kept as advice", got)
	}
	for _, f := range got {
		if f.RecommendedSQL != "" || f.Severity != "info" {
			t.Errorf("finding %q is executable without a window", f.ObjectIdentifier)
		}
	}
	if !strings.Contains(prompts[0].user, "Lowest max_wal_size: unknown") {
		t.Errorf("prompt claims a floor without history:\n%s", prompts[0].user)
	}
}

// CHECK-05 (property): over random facts, current values and model values,
// no emitted max_wal_size statement is below the current value or the
// floor, or above 4 x floor.
func TestWALFloor_PropertyNeverBelowCurrentOrFloor(t *testing.T) {
	r := rand.New(rand.NewSource(20261111))
	for i := 0; i < 2000; i++ {
		perTimeout := float64(r.Int63n(40 << 30))
		f := walWindowFacts{known: true, walPerTimeout: &perTimeout,
			checkpointTimeout: 5 * time.Minute, measured: time.Hour}
		if r.Intn(5) == 0 {
			f.walPerTimeout = nil
		}
		fl := walSizeFloor(f)
		current := float64((512 + r.Int63n(60<<10)) << 20)
		model := fmt.Sprintf("%dMB", 512+r.Int63n(64<<10-512))
		findings := parseLLMFindings(walModelAnswer(model), "wal_tuning", noopLog)
		out := filterUnsafeMaxWALSize(findings, walCurrent{bytes: current, known: true},
			fl, f, noopLog)
		for _, o := range out {
			v, ok := maxWALTarget(t, o)
			if !ok {
				continue
			}
			if v < current || !fl.known || v < fl.bytes || v > walCeilingFactor*fl.bytes {
				t.Fatalf("case %d: emitted %.0f with current %.0f floor %+v (model %s)",
					i, v, current, fl, model)
			}
		}
	}
}

// The current max_wal_size is read in the server's unit; a missing or
// unparseable setting is unknown, never 0.
func TestCurrentMaxWALSize(t *testing.T) {
	cases := []struct {
		settings []collector.PGSetting
		want     float64
		known    bool
	}{
		{[]collector.PGSetting{{Name: "max_wal_size", Setting: "1024", Unit: "MB"}},
			1 << 30, true},
		{[]collector.PGSetting{{Name: "max_wal_size", Setting: "x", Unit: "MB"}}, 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got := currentMaxWALSize(c.settings)
		if got.known != c.known || got.bytes != c.want {
			t.Errorf("currentMaxWALSize(%v) = %+v, want %.0f known=%v", c.settings, got,
				c.want, c.known)
		}
	}
}
