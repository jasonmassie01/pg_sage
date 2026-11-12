package earned

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// earned-07: a self-initiated level re-validates after a PostgreSQL major
// upgrade. The major comes from server_version_num, read from the
// monitored database at most once per MajorProbeTTL.

func TestParseServerVersionNumMatrix(t *testing.T) {
	for setting, want := range map[string]int{
		"140000": 14, "150013": 15, "160004": 16, "170000": 17, "180001": 18,
		"170002": 17, "100000": 10, " 170000\n": 17,
	} {
		got, err := ParseServerVersionNum(setting)
		if err != nil || got != want {
			t.Errorf("ParseServerVersionNum(%q) = %d, %v; want %d", setting, got, err, want)
		}
	}
}

func TestParseServerVersionNumRejectsInvalid(t *testing.T) {
	for _, setting := range []string{"", "  ", "abc", "17.0", "17", "-170000", "0",
		"90624", "1e6", "170000x", "99999999", "170000; DROP TABLE x"} {
		got, err := ParseServerVersionNum(setting)
		if err == nil || got != 0 {
			t.Errorf("ParseServerVersionNum(%q) = %d, %v; want an error", setting, got, err)
		}
		if err != nil && !strings.Contains(err.Error(), "server_version_num") {
			t.Errorf("error for %q does not name the setting: %v", setting, err)
		}
	}
}

// A minor upgrade keeps the major: no re-validation.
func TestMinorUpgradeKeepsTheMajor(t *testing.T) {
	before, _ := ParseServerVersionNum("170000")
	after, _ := ParseServerVersionNum("170002")
	if before != after || before != 17 {
		t.Fatalf("minor upgrade changed the major: %d -> %d", before, after)
	}
}

type fakeVersionRow struct {
	val string
	err error
}

func (r fakeVersionRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("want one column")
	}
	p, ok := dest[0].(*string)
	if !ok {
		return errors.New("want a *string")
	}
	*p = r.val
	return nil
}

// fakeVersionDB answers server_version_num with val (or err) and counts
// the probes.
type fakeVersionDB struct {
	mu    sync.Mutex
	val   string
	err   error
	calls int
	sql   string
}

func (f *fakeVersionDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.sql = sql
	return fakeVersionRow{val: f.val, err: f.err}
}

func (f *fakeVersionDB) set(val string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.val, f.err = val, err
}

func (f *fakeVersionDB) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newTestProbe(db *fakeVersionDB, clk *clock) (*MajorProbe, *[]string) {
	var logs []string
	var mu sync.Mutex
	p := newMajorProbe(db, MajorProbeTTL, clk.Now, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, format)
	})
	return p, &logs
}

func TestMajorProbeReadsAndCaches(t *testing.T) {
	db := &fakeVersionDB{val: "160004"}
	clk := newClock(fixtureEpoch)
	p, _ := newTestProbe(db, clk)
	ctx := context.Background()
	if got, err := p.Major(ctx); err != nil || got != 16 {
		t.Fatalf("first read = %d, %v", got, err)
	}
	if !strings.Contains(db.sql, "current_setting('server_version_num')") {
		t.Fatalf("probe SQL = %q", db.sql)
	}
	db.set("170000", nil)
	clk.Advance(MajorProbeTTL - time.Second)
	if got, _ := p.Major(ctx); got != 16 || db.count() != 1 {
		t.Fatalf("inside the TTL = %d after %d probes; want the cached 16, 1 probe", got,
			db.count())
	}
	clk.Advance(2 * time.Second)
	if got, err := p.Major(ctx); err != nil || got != 17 || db.count() != 2 {
		t.Fatalf("after the TTL = %d, %v after %d probes", got, err, db.count())
	}
}

// An unreadable version keeps the last known major and logs; with none
// known it is an error (the limiter then does not cap).
func TestMajorProbeKeepsTheLastKnownMajor(t *testing.T) {
	db := &fakeVersionDB{err: errors.New("connection refused")}
	clk := newClock(fixtureEpoch)
	p, logs := newTestProbe(db, clk)
	ctx := context.Background()
	got, err := p.Major(ctx)
	if err == nil || got != 0 || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("no major known = %d, %v", got, err)
	}
	db.set("160004", nil)
	clk.Advance(time.Second) // a failed read is retried, not cached
	if got, err := p.Major(ctx); err != nil || got != 16 {
		t.Fatalf("recovered read = %d, %v", got, err)
	}
	db.set("not-a-number", nil)
	clk.Advance(MajorProbeTTL + time.Second)
	if got, err := p.Major(ctx); err != nil || got != 16 {
		t.Fatalf("non-numeric setting = %d, %v; want the last known 16", got, err)
	}
	db.set("", errors.New("timeout"))
	clk.Advance(MajorProbeTTL + time.Second)
	if got, err := p.Major(ctx); err != nil || got != 16 {
		t.Fatalf("failed read = %d, %v; want the last known 16", got, err)
	}
	if len(*logs) < 3 {
		t.Fatalf("logged %d failures, want 3: %v", len(*logs), *logs)
	}
}

func TestMajorProbeNilIsAnError(t *testing.T) {
	var p *MajorProbe
	if got, err := p.Major(context.Background()); err == nil || got != 0 {
		t.Fatalf("nil probe = %d, %v", got, err)
	}
	if got, err := NewMajorProbe(nil, nil).Major(context.Background()); err == nil ||
		got != 0 {
		t.Fatalf("probe without a pool = %d, %v", got, err)
	}
}

// Parallel callers inside the TTL share one probe.
func TestMajorProbeConcurrentCallersShareOneRead(t *testing.T) {
	db := &fakeVersionDB{val: "170000"}
	p, _ := newTestProbe(db, newClock(fixtureEpoch))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := p.Major(context.Background()); err != nil || got != 17 {
				errs <- errors.New("wrong major")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if db.count() != 1 {
		t.Fatalf("%d probes for 16 parallel callers, want 1", db.count())
	}
}

func TestMajorCapText(t *testing.T) {
	got := MajorCapText(16, 17, 1, 3)
	want := "capped at L2: PostgreSQL 16→17; 1 of 3 improved outcomes, or re-approve"
	if got != want {
		t.Fatalf("MajorCapText = %q, want %q", got, want)
	}
}

// The cap event names the change; its evidence round-trips, and the
// cap's clearing is a cap_cleared.
func TestMajorCapEventRoundTrip(t *testing.T) {
	next := capState{major: &majorNote{From: 16, To: 17}}
	if next.key() != "server_major_changed:16->17" {
		t.Fatalf("key = %q", next.key())
	}
	e := capEvent(FamilyHygiene, ClassVacuum, capState{}, next, fixtureEpoch)
	if e.Type != EventCapped || e.Reason != "server_major_changed:16->17" {
		t.Fatalf("cap event = %s %q", e.Type, e.Reason)
	}
	back := capStateOf(&e)
	if back.major == nil || back.major.From != 16 || back.major.To != 17 ||
		back.key() != next.key() {
		t.Fatalf("round trip = %+v", back)
	}
	cleared := capEvent(FamilyHygiene, ClassVacuum, back, capState{}, fixtureEpoch)
	if cleared.Type != EventCapCleared ||
		!strings.Contains(cleared.Reason, "PostgreSQL 16→17 re-validated") {
		t.Fatalf("clear event = %s %q", cleared.Type, cleared.Reason)
	}
	if capStateOf(&cleared).major != nil {
		t.Fatal("a cap_cleared still carries the major cap")
	}
	both := capState{downs: []Downgrade{{DowngradeHARole, "role replica"}},
		major: &majorNote{From: 16, To: 17}}
	e = capEvent(FamilyHygiene, ClassVacuum, capState{}, both, fixtureEpoch)
	if e.Type != EventCapped || !strings.Contains(e.Reason, "at most L1") ||
		capStateOf(&e).major == nil {
		t.Fatalf("downgrade and major cap = %s %q %+v", e.Type, e.Reason, capStateOf(&e))
	}
}
