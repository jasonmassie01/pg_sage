package backtest

import (
	"fmt"
	"math"
	"time"
)

// The five bench fixtures (lab-02): deterministic histories with no real
// step anywhere, each built to trip a naive verifier one way. Six tables
// of two statements each (a cheap and an expensive one), a 5-minute
// collector cycle and FixtureDays of history, every cycle's counters
// wobbling by a seeded hash so a fixture is the same on every machine.
const (
	FixtureDays  = 35
	FixtureCycle = 5 * time.Minute
	fixtureSeed  = 0x6c6162
)

// FixtureStart is the first day of every fixture, a Monday (UTC).
var FixtureStart = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

// Fixture names.
const (
	FixtureSeasonalWeek   = "seasonal-week"
	FixtureTrafficShift   = "traffic-shift"
	FixtureNoisyNeighbour = "noisy-neighbour"
	FixtureDeployBreaks   = "deploy-breaks"
	FixtureWeeklyOnly     = "weekly-only-targets"
)

// FixtureNames are the fixtures in the order the docs list them.
var FixtureNames = []string{FixtureSeasonalWeek, FixtureTrafficShift,
	FixtureNoisyNeighbour, FixtureDeployBreaks, FixtureWeeklyOnly}

// fixtureStmt is one statement: its table, calls per cycle, and blocks and
// milliseconds per call before the fixture's shape applies.
type fixtureStmt struct {
	id                   int64
	table                string
	calls, blocks, msPer float64
}

func fixtureStmts() []fixtureStmt {
	var out []fixtureStmt
	for k := int64(1); k <= 6; k++ {
		table := fmt.Sprintf("public.t%d", k)
		out = append(out,
			fixtureStmt{id: 100*k + 1, table: table, calls: 40 + 2*float64(k),
				blocks: 100, msPer: 1},
			fixtureStmt{id: 100*k + 2, table: table, calls: 20 + float64(k),
				blocks: 400, msPer: 8})
	}
	return out
}

// shape is a fixture's multipliers of calls, blocks per call and time per
// call of s in the cycle ending at t, and whether t is an epoch break.
type shape struct {
	at    func(s fixtureStmt, t time.Time) (calls, blocks, ms float64)
	broke func(t time.Time) bool
}

// unit is a deterministic value in [-1, 1) of its keys (splitmix64).
func unit(keys ...int64) float64 {
	x := uint64(fixtureSeed)
	for _, k := range keys {
		x ^= uint64(k)
		x += 0x9e3779b97f4a7c15
		x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
		x = (x ^ (x >> 27)) * 0x94d049bb133111eb
		x ^= x >> 31
	}
	return float64(x>>11)/float64(1<<52) - 1
}

// diurnal is the day's traffic curve: low at night, high in the afternoon.
func diurnal(t time.Time) float64 {
	h := float64(t.Hour()) + float64(t.Minute())/60
	return 1 + 0.6*math.Sin(2*math.Pi*(h-9)/24)
}

func weekend(t time.Time) bool {
	return t.Weekday() == time.Saturday || t.Weekday() == time.Sunday
}

func tableNo(s fixtureStmt) int64 { return s.id / 100 }

// build generates the fixture's cycles from start: days of them, one
// every cycle. A statement's calls are the sum of its cycles' calls.
func build(name string, start time.Time, days int, every time.Duration, sh shape) *Memory {
	specs := fixtureStmts()
	ivs := map[int64][]Interval{}
	var breaks []time.Time
	calls := map[int64]int64{}
	n := days * int(day/every)
	for i := 1; i <= n; i++ {
		t := start.Add(time.Duration(i) * every)
		if sh.broke != nil && sh.broke(t) {
			breaks = append(breaks, t)
		}
		for _, s := range specs {
			iv := cycle(s, t, int64(i), sh)
			ivs[s.id] = append(ivs[s.id], iv)
			calls[s.id] += iv.Calls
		}
	}
	stmts := make([]Statement, 0, len(specs))
	for _, s := range specs {
		stmts = append(stmts, Statement{QueryID: s.id, Tables: []string{s.table},
			Calls: calls[s.id]})
	}
	return NewMemory(name, start, stmts, nil, ivs, breaks)
}

func cycle(s fixtureStmt, t time.Time, i int64, sh shape) Interval {
	cm, bm, mm := sh.at(s, t)
	calls := int64(math.Round(s.calls * cm * (1 + 0.15*unit(s.id, i, 1))))
	if calls < 0 {
		calls = 0
	}
	c := float64(calls)
	return Interval{To: t, Calls: calls, Rows: calls,
		Blocks: int64(math.Round(c * s.blocks * bm * (1 + 0.05*unit(s.id, i, 2)))),
		Ms:     math.Round(c * s.msPer * mm * (1 + 0.08*unit(s.id, i, 3)))}
}

// Fixture builds the named fixture from FixtureStart over FixtureDays at
// FixtureCycle; an unknown name is an error.
func Fixture(name string) (*Memory, error) {
	return FixtureAt(name, FixtureStart, FixtureDays, FixtureCycle)
}

// FixtureAt builds the named fixture from start over days, one cycle
// every every (whole milliseconds and blocks, so counters summed and
// differenced in a database give the same values back).
func FixtureAt(name string, start time.Time, days int, every time.Duration) (*Memory,
	error) {
	sh, ok := shapes()[name]
	switch {
	case !ok:
		return nil, fmt.Errorf("unknown bench fixture %q", name)
	case days <= 0 || every <= 0 || every > day:
		return nil, fmt.Errorf("bench fixture %q needs positive days and a cycle of at "+
			"most a day, got %d days every %s", name, days, every)
	}
	return build(name, start.UTC(), days, every, sh), nil
}

func shapes() map[string]shape {
	return map[string]shape{
		FixtureSeasonalWeek:   {at: seasonalWeek},
		FixtureTrafficShift:   {at: trafficShift},
		FixtureNoisyNeighbour: {at: noisyNeighbour},
		FixtureDeployBreaks: {at: func(fixtureStmt, time.Time) (float64, float64, float64) {
			return 1, 1, 1
		}, broke: deployBreak},
		FixtureWeeklyOnly: {at: weeklyOnly},
	}
}

// seasonalWeek: traffic follows the day and drops at weekends, and the
// first two tables scan 40% more per call at weekends (a batch) with a
// daily swing: a pre/post comparison sees changes the same hour of
// earlier weeks explains.
func seasonalWeek(s fixtureStmt, t time.Time) (float64, float64, float64) {
	calls, blocks := diurnal(t), 1.0
	if weekend(t) {
		calls *= 0.6
	}
	if tableNo(s) <= 2 {
		blocks = 1 + 0.2*math.Sin(2*math.Pi*float64(t.Hour())/24)
		if weekend(t) {
			blocks *= 1.4
		}
	}
	return calls, blocks, 1
}

// trafficShift: on the first three tables the mix of the cheap and the
// expensive statement flips at random 12-hour marks (each statement's own
// cost never changes): a call-weighted mean of time per call moves by
// three times while nothing got faster or slower.
func trafficShift(s fixtureStmt, t time.Time) (float64, float64, float64) {
	if tableNo(s) > 3 {
		return 1, 1, 1
	}
	heavy := unit(tableNo(s), t.Unix()/int64(12*time.Hour/time.Second), 4) > 0
	if heavy == (s.id%100 == 1) {
		return 1.6, 1, 1
	}
	return 0.4, 1, 1
}

// noisyNeighbour: the sixth table's statements burst hour by hour, in
// calls and in blocks per call, so a control group that holds them has
// one member far noisier than the rest.
func noisyNeighbour(s fixtureStmt, t time.Time) (float64, float64, float64) {
	if tableNo(s) != 6 {
		return 1, 1, 1
	}
	hour := t.Unix() / 3600
	return math.Exp(0.7 * unit(s.id, hour, 5)), math.Exp(0.7 * unit(s.id, hour, 6)), 1
}

// deployBreak: statistics reset at each deploy, Tuesdays and Fridays at
// 14:00 (as a Neon or Lakebase suspend and resume does): placebos whose
// windows cross one are excluded, never judged.
func deployBreak(t time.Time) bool {
	day := t.Weekday() == time.Tuesday || t.Weekday() == time.Friday
	return day && t.Hour() == 14 && t.Minute() == 0
}

// weeklyOnly: the last two tables run only on Sundays from 02:00 to
// 04:00 (a weekly report), so most placebos on them see no calls.
func weeklyOnly(s fixtureStmt, t time.Time) (float64, float64, float64) {
	if tableNo(s) < 5 {
		return diurnal(t), 1, 1
	}
	if t.Weekday() == time.Sunday && t.Hour() >= 2 && t.Hour() < 4 {
		return 6, 1, 1
	}
	return 0, 1, 1
}
