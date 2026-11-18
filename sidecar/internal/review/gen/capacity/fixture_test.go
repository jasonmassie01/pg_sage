package capacity

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/gen/capacity"))
}

const (
	gib     = float64(1 << 30)
	mib     = float64(1 << 20)
	dbID    = "growth/db/size"
	tableID = "growth/table/public.orders"
)

// window is the 28-day review window of the tests.
var window = reviewtest.Window(28)

// testEnv is the review environment of the tests.
func testEnv() candidate.Env { return candidate.Env{Database: "app", WindowDays: 28} }

// linear is n daily sizes from start, perDay apart, each moved by noise
// up on even days and down on odd ones.
func linear(n int, start, perDay, noise float64) []*float64 {
	out := make([]*float64, n)
	for i := range out {
		v := start + perDay*float64(i)
		if i%2 == 0 {
			v += noise
		} else {
			v -= noise
		}
		out[i] = &v
	}
	return out
}

// sparse keeps only the listed indexes of values (the others are missing).
func sparse(values []*float64, keep ...int) []*float64 {
	out := make([]*float64, len(values))
	for _, k := range keep {
		out[k] = values[k]
	}
	return out
}

func series(id, table string, values []*float64) workpack.Series {
	return workpack.Series{ID: id, Table: table, Unit: "bytes", Values: values}
}

// growthPack is a 28-day pack whose growth section holds db and tables.
func growthPack(t *testing.T, db workpack.Series, tables ...workpack.Series) *workpack.Pack {
	t.Helper()
	p, err := reviewtest.Build(reviewtest.Spec{Database: "app", Window: window,
		Growth: &workpack.Growth{DBSize: db, Tables: tables}})
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

// steadyPack is a pack whose database grows 1 GiB a day from 100 GiB.
func steadyPack(t *testing.T) *workpack.Pack {
	return growthPack(t, series(dbID, "", linear(28, 100*gib, gib, mib)))
}

// quiet is n observed days ending at the window's end: the first active
// of them had workload calls, the rest none.
func quiet(n, active int) []DayCalls {
	out := make([]DayCalls, n)
	start := window.To.AddDate(0, 0, -n)
	for i := range out {
		out[i] = DayCalls{Day: start.AddDate(0, 0, i), Observed: true}
		if i < active {
			out[i].WorkloadCalls = 100
		}
	}
	return out
}

// byKind is the candidates of kind.
func byKind(cands []candidate.Candidate, kind string) []candidate.Candidate {
	var out []candidate.Candidate
	for _, c := range cands {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// requireNoDollars fails when the JSON of the output holds any money: a
// currency, a dollar sign or a cost quantity.
func requireNoDollars(t *testing.T, cands []candidate.Candidate, cov candidate.Coverage) {
	t.Helper()
	raw, err := json.Marshal(struct {
		C []candidate.Candidate
		V candidate.Coverage
	}{cands, cov})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, bad := range []string{"$", "USD", "EUR", QuantityMonthlyCost,
		QuantityMonthlyDelta, "per month"} {
		if strings.Contains(s, bad) {
			t.Fatalf("the output without rates holds %q:\n%s", bad, s)
		}
	}
}

// day is the UTC midnight d days after the window's start.
func day(d int) time.Time { return window.From.AddDate(0, 0, d) }

// hasNote reports a note holding every part.
func hasNote(notes []string, parts ...string) bool {
	for _, n := range notes {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(n, p)
		}
		if ok {
			return true
		}
	}
	return false
}
