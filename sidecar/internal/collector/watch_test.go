package collector

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// collection-07: the queries step admits, besides the top statements by
// total time, the slowest by mean, the heaviest by I/O and the pinned
// queryids (open verification targets and in-memory watches). These tests
// need no database: armStatements answers the counters read with rows
// that carry their admission, and records the arguments it was sent.

// pinFake is a PinSource returning ids, or err, and counting its calls.
type pinFake struct {
	mu    sync.Mutex
	ids   []int64
	err   error
	calls int
}

func (p *pinFake) PinnedQueryIDs(context.Context) ([]int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return slices.Clone(p.ids), nil
}

func (p *pinFake) set(ids []int64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids, p.err = ids, err
}

// armStatements is fakeStatements that records the counters reads.
type armStatements struct {
	*fakeStatements
	mu   sync.Mutex
	sql  []string
	args [][]any
}

func (a *armStatements) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(sql, `pg_stat_statements"(true)`) {
		a.mu.Lock()
		a.sql = append(a.sql, sql)
		a.args = append(a.args, slices.Clone(args))
		a.mu.Unlock()
	}
	return a.fakeStatements.Query(ctx, sql, args...)
}

func (a *armStatements) last(t *testing.T) (string, []any) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sql) == 0 {
		t.Fatal("no counters read was sent")
	}
	return a.sql[len(a.sql)-1], a.args[len(a.args)-1]
}

// armRow is a counters row admitted by arms.
func armRow(id int64, total float64, arms ...string) []any {
	row := counterRow(id, total)
	row[len(row)-1] = arms
	return row
}

func newArmStatements(rows [][]any, ids ...int64) *armStatements {
	f := newFakeStatements(appTexts(ids...))
	f.counters = rows
	return &armStatements{fakeStatements: f}
}

// Boundary: the arm size K is a tenth of N, at least 1 and at most 50;
// the pins never exceed 64 nor leave the total arm fewer than N rows; the
// rows read never exceed 2N. N = 1 is too small to share: today's read.
func TestAdmissionFor_Sizes(t *testing.T) {
	cases := []struct{ n, perArm, pinCap int }{
		{1, 0, 0}, {2, 1, 0}, {3, 1, 1}, {9, 1, 7}, {10, 1, 8}, {11, 1, 9},
		{100, 10, 64}, {499, 49, 64}, {500, 50, 64}, {501, 50, 64}, {5000, 50, 64},
	}
	for _, tc := range cases {
		a := admissionFor(tc.n)
		if a.perArm != tc.perArm || a.pinCap != tc.pinCap {
			t.Errorf("N=%d: perArm %d pinCap %d, want %d and %d",
				tc.n, a.perArm, a.pinCap, tc.perArm, tc.pinCap)
		}
		for _, pins := range []int{0, a.pinCap} {
			if got := a.total(pins); got < tc.n {
				t.Errorf("N=%d pins %d: total arm %d < N", tc.n, pins, got)
			}
			if got := a.candidates(pins); got != 2*tc.n {
				t.Errorf("N=%d pins %d: %d rows read, want 2N = %d",
					tc.n, pins, got, 2*tc.n)
			}
		}
	}
}

// Boundary: 100 watches against P = 64 admit the 64 oldest, in age order.
func TestPinnedQueryIDs_CapKeepsOldestWatches(t *testing.T) {
	c, clock := textCollector(500)
	start := *clock
	for i := 0; i < 100; i++ {
		*clock = start.Add(time.Duration(i) * time.Second)
		c.Watch(int64(1000+i), start.Add(time.Hour))
	}
	got := c.pinnedQueryIDs(context.Background(), admissionFor(500).pinCap)
	if len(got) != pinCapMax || pinCapMax != 64 {
		t.Fatalf("%d pins (cap %d), want 64", len(got), pinCapMax)
	}
	for i, id := range got {
		if id != int64(1000+i) {
			t.Fatalf("pin %d = %d, want %d (oldest first): %v", i, id, 1000+i, got)
		}
	}
}

// Durable pins come first, in the source's order, then the watches; an id
// in both is pinned once.
func TestPinnedQueryIDs_DurableFirstDeduplicated(t *testing.T) {
	c, _ := textCollector(500)
	c.WithPinSource(&pinFake{ids: []int64{7, 5, 7, 9, 0}})
	c.Watch(11, c.clock().Add(time.Hour))
	c.Watch(5, c.clock().Add(time.Hour))
	got := c.pinnedQueryIDs(context.Background(), 64)
	if want := []int64{7, 5, 9, 11}; !slices.Equal(got, want) {
		t.Fatalf("pins = %v, want %v", got, want)
	}
	if got := c.pinnedQueryIDs(context.Background(), 2); !slices.Equal(got, []int64{7, 5}) {
		t.Fatalf("pins capped at 2 = %v, want [7 5]", got)
	}
	if got := c.pinnedQueryIDs(context.Background(), 0); len(got) != 0 {
		t.Fatalf("pins capped at 0 = %v, want none", got)
	}
}

// A source that hands out a slice of its own state keeps it: the watches
// are not appended into the spare capacity of the source's slice.
func TestPinnedQueryIDs_LeavesTheSourceSliceAlone(t *testing.T) {
	c, clock := textCollector(500)
	backing := []int64{1, 2, 0, 0}
	c.WithPinSource(PinSourceFunc(func(context.Context) ([]int64, error) {
		return backing[:2], nil
	}))
	c.Watch(9, clock.Add(time.Hour))
	if got := c.pinnedQueryIDs(context.Background(), 64); !slices.Equal(got, []int64{1, 2, 9}) {
		t.Fatalf("pins = %v, want [1 2 9]", got)
	}
	if !slices.Equal(backing, []int64{1, 2, 0, 0}) {
		t.Fatalf("the source's slice was written to: %v", backing)
	}
}

// State transition: a watch admits its queryid until it expires (fake
// clock), then never again; re-watching extends it and keeps its age.
func TestWatch_ExpiresOnTheClock(t *testing.T) {
	c, clock := textCollector(500)
	start := *clock
	c.Watch(42, start.Add(10*time.Minute))
	c.Watch(43, start.Add(time.Minute))
	c.Watch(43, start.Add(20*time.Minute))
	*clock = start.Add(9 * time.Minute)
	if got := c.pinnedQueryIDs(context.Background(), 64); !slices.Equal(got, []int64{42, 43}) {
		t.Fatalf("before expiry: %v, want [42 43]", got)
	}
	*clock = start.Add(10 * time.Minute)
	if got := c.pinnedQueryIDs(context.Background(), 64); !slices.Equal(got, []int64{43}) {
		t.Fatalf("at 42's expiry: %v, want [43]", got)
	}
	*clock = start.Add(20 * time.Minute)
	if got := c.pinnedQueryIDs(context.Background(), 64); len(got) != 0 {
		t.Fatalf("after every expiry: %v, want none", got)
	}
	*clock = start
	if got := c.pinnedQueryIDs(context.Background(), 64); len(got) != 0 {
		t.Fatalf("an expired watch came back when the clock went back: %v", got)
	}
}

// Invalid input: queryid 0 (unattributable) and a watch already expired
// are not registered; a shorter re-watch never shortens a watch.
func TestWatch_IgnoresInvalidWatches(t *testing.T) {
	c, clock := textCollector(500)
	c.Watch(0, clock.Add(time.Hour))
	c.Watch(5, clock.Add(-time.Second))
	c.Watch(6, *clock)
	if got := c.pinnedQueryIDs(context.Background(), 64); len(got) != 0 {
		t.Fatalf("invalid watches admitted: %v", got)
	}
	c.Watch(8, clock.Add(time.Hour))
	c.Watch(8, clock.Add(time.Minute))
	*clock = clock.Add(30 * time.Minute)
	if got := c.pinnedQueryIDs(context.Background(), 64); !slices.Equal(got, []int64{8}) {
		t.Fatalf("a shorter re-watch shortened the watch: %v", got)
	}
}

// Nil/empty: no source and an empty source send the same statement with
// empty (not NULL) arrays, and return today's top-N by total time.
func TestCollectQueries_NoPinsSendEmptyArrays(t *testing.T) {
	rows := [][]any{armRow(3, 300, "total"), armRow(2, 200, "total"), armRow(1, 100, "total")}
	var sqls []string
	var argss [][]any
	for _, src := range []PinSource{nil, &pinFake{ids: []int64{}}, &pinFake{}} {
		c, _ := textCollector(2)
		if src != nil {
			c.WithPinSource(src)
		}
		db := newArmStatements(rows, 1, 2, 3)
		qs := mustCollect(t, c, db)
		if got := idsOf(qs); !slices.Equal(got, []int64{3, 2}) {
			t.Fatalf("source %T: ids %v, want the top 2 by total [3 2]", src, got)
		}
		sql, args := db.last(t)
		sqls, argss = append(sqls, sql), append(argss, args)
	}
	for i, args := range argss {
		if len(args) != 2 {
			t.Fatalf("case %d: %d arguments, want 2 (pins, own ids)", i, len(args))
		}
		for j, arg := range args {
			ids, ok := arg.([]int64)
			if !ok || ids == nil || len(ids) != 0 {
				t.Errorf("case %d arg %d = %#v, want an empty non-nil []int64", i, j, arg)
			}
		}
		if sqls[i] != sqls[0] || !reflect.DeepEqual(args, argss[0]) {
			t.Errorf("case %d sent a different statement or arguments", i)
		}
	}
}

// Error propagation: a failing source leaves the step working, warns once
// per failure streak, and pins nothing; a watch still pins.
func TestCollectQueries_PinSourceFailureWarnsOncePerStreak(t *testing.T) {
	c, clock := textCollector(5)
	logs := &logRecorder{}
	c.logFn = logs.log
	src := &pinFake{err: errors.New("relation \"sage.verification\" does not exist")}
	c.WithPinSource(src)
	db := newArmStatements([][]any{armRow(1, 100, "total")}, 1)
	for i := 0; i < 3; i++ {
		qs, err := c.collectQueriesVia(context.Background(), db)
		if err != nil || !slices.Equal(idsOf(qs), []int64{1}) {
			t.Fatalf("cycle %d: %v, %v; want the step to succeed with [1]", i, idsOf(qs), err)
		}
		if _, args := db.last(t); len(args[0].([]int64)) != 0 {
			t.Fatalf("cycle %d pinned %v from a failing source", i, args[0])
		}
	}
	if n := logs.count("WARN", "sage.verification"); n != 1 || src.calls != 3 {
		t.Fatalf("%d WARNs over %d failing reads, want 1 over 3", n, src.calls)
	}
	src.set([]int64{77}, nil)
	mustCollect(t, c, db)
	if _, args := db.last(t); !slices.Equal(args[0].([]int64), []int64{77}) {
		t.Fatalf("recovered source pinned %v, want [77]", args[0])
	}
	src.set(nil, errors.New("connection reset by peer"))
	c.Watch(88, clock.Add(time.Hour))
	mustCollect(t, c, db)
	if _, args := db.last(t); !slices.Equal(args[0].([]int64), []int64{88}) {
		t.Fatalf("watch during a failure pinned %v, want [88]", args[0])
	}
	if n := logs.count("WARN", "pinned"); n != 2 {
		t.Fatalf("%d WARNs after a second streak, want 2:\n%v", n, logs.lines)
	}
}

// Boundary: with N = 10 and 100 watches, 8 are pinned (N - 2K), the read
// asks for 2N rows, and no more than N queries are published even when
// the server returned more.
func TestCollectQueries_PinsNeverExceedTheBound(t *testing.T) {
	c, clock := textCollector(10)
	for i := 0; i < 100; i++ {
		c.Watch(int64(500+i), clock.Add(time.Hour))
	}
	var rows [][]any
	var ids []int64
	for i := 0; i < 30; i++ {
		id := int64(500 + i)
		rows, ids = append(rows, armRow(id, float64(1000-i), "pinned")), append(ids, id)
	}
	db := newArmStatements(rows, ids...)
	qs := mustCollect(t, c, db)
	if len(qs) != 10 {
		t.Fatalf("%d queries published, want N = 10", len(qs))
	}
	sql, args := db.last(t)
	if pins := args[0].([]int64); len(pins) != 8 || pins[0] != 500 || pins[7] != 507 {
		t.Fatalf("pins sent = %v, want the 8 oldest 500..507", pins)
	}
	if !strings.HasSuffix(strings.TrimSpace(sql), "LIMIT 20") {
		t.Fatalf("read does not ask for 2N = 20 rows:\n%s", sql)
	}
	if !strings.Contains(sql, "LIMIT 10)") || strings.Count(sql, "LIMIT 1)") != 2 {
		t.Errorf("arm sizes: want total 20-2-8 = 10 and K = 1 twice:\n%s", sql)
	}
}

// Happy path: the rows the read admits beyond the total ranking (pinned,
// mean, io) survive the cut at N, and the queries are published by total
// time, with their admission.
func TestCollectQueries_AdmittedRowsSurviveTheCut(t *testing.T) {
	c, _ := textCollector(3)
	rows := [][]any{
		armRow(9, 1, "pinned"), armRow(8, 2, "mean"), armRow(7, 3, "io", "total"),
		armRow(1, 900, "total"), armRow(2, 800, "total"), armRow(3, 700, "total"),
	}
	db := newArmStatements(rows, 1, 2, 3, 7, 8, 9)
	qs := mustCollect(t, c, db)
	if got := idsOf(qs); !slices.Equal(got, []int64{7, 8, 9}) {
		t.Fatalf("ids %v, want the admitted [7 8 9] by total time", got)
	}
	want := [][]string{{"io", "total"}, {"mean"}, {"pinned"}}
	for i, q := range qs {
		if !slices.Equal(q.AdmittedBy, want[i]) {
			t.Errorf("query %d admitted_by %v, want %v", q.QueryID, q.AdmittedBy, want[i])
		}
	}
}

// pg_sage's own statements whose texts the cache holds are kept out of the
// mean and io arms, which would otherwise fill with pg_sage's catalog reads.
func TestCollectQueries_OwnStatementsLeftOutOfTheArms(t *testing.T) {
	c, _ := textCollector(5)
	texts := appTexts(1)
	texts[66] = "SELECT * FROM sage.findings WHERE id = $1"
	f := newFakeStatements(texts)
	f.counters = [][]any{armRow(1, 100, "total"), armRow(66, 50, "total")}
	db := &armStatements{fakeStatements: f}
	if got := idsOf(mustCollect(t, c, db)); !slices.Equal(got, []int64{1}) {
		t.Fatalf("first cycle: %v, want [1]", got)
	}
	mustCollect(t, c, db)
	if _, args := db.last(t); !slices.Equal(args[1].([]int64), []int64{66}) {
		t.Fatalf("own ids sent = %v, want [66]", args[1])
	}
}

// The admission is published only when known.
func TestQueryStats_AdmittedByJSON(t *testing.T) {
	plain, err := json.Marshal(QueryStats{QueryID: 1})
	if err != nil || strings.Contains(string(plain), "admitted_by") {
		t.Fatalf("no admission marshals as %s (%v), want no admitted_by", plain, err)
	}
	with, err := json.Marshal(QueryStats{QueryID: 1, AdmittedBy: []string{"mean"}})
	if err != nil || !strings.Contains(string(with), `"admitted_by":["mean"]`) {
		t.Fatalf("admission marshals as %s (%v)", with, err)
	}
}

// Concurrent access: the executor's goroutines watch while the collector
// reads its pins (-race is unavailable on this machine: no cgo; the test
// checks that every watch lands and no read sees a torn registry).
func TestWatch_ConcurrentWithPinReads(t *testing.T) {
	c, _ := textCollector(500)
	until := c.clock().Add(time.Hour)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				c.Watch(int64(w*1000+i+1), until)
			}
		}(w)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if got := c.pinnedQueryIDs(context.Background(), 64); len(got) > 64 {
				t.Errorf("read %d pins past the cap", len(got))
				return
			}
		}
	}()
	wg.Wait()
	<-done
	if got := c.pinnedQueryIDs(context.Background(), 1000); len(got) != 800 {
		t.Fatalf("%d watches registered, want 800", len(got))
	}
}
