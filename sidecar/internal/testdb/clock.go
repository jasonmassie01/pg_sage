package testdb

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The test server often runs in a VM (Docker Desktop) whose clock slews
// against the host's by seconds within minutes. Compare times on the
// clock that produced them: a stamp compared with a database timestamp
// comes from DBNow, and a bound on an interval the database measures is
// widened by the step a ClockWatch measured, never by a constant.

const dbNowSQL = `/* pg_sage test */ SELECT clock_timestamp()`

// clockSampleTimeout bounds one clock read from a test helper.
const clockSampleTimeout = 10 * time.Second

// DBNow reads the database server's clock: clock_timestamp(), which moves
// inside a transaction, unlike now().
func DBNow(ctx context.Context, q Querier) (time.Time, error) {
	if isNilQuerier(q) {
		return time.Time{}, errors.New(
			"testdb: read the database clock (clock_timestamp()): no querier")
	}
	var at time.Time
	if err := q.QueryRow(ctx, dbNowSQL).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf(
			"testdb: read the database clock (clock_timestamp()): %w", err)
	}
	return at, nil
}

// MustDBNow is DBNow for a test: a failed read fails t.
func MustDBNow(t testing.TB, q Querier) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), clockSampleTimeout)
	defer cancel()
	at, err := DBNow(ctx, q)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return at
}

// clockSample is one reading of both clocks: host carries Go's monotonic
// reading, db the server's clock_timestamp().
type clockSample struct {
	host time.Time
	db   time.Time
}

type clockSampler func(ctx context.Context) (clockSample, error)

// querierSampler stamps the host at the midpoint of the round trip, so
// the query's latency does not count as a step.
func querierSampler(q Querier) clockSampler {
	return func(ctx context.Context) (clockSample, error) {
		before := time.Now()
		db, err := DBNow(ctx, q)
		if err != nil {
			return clockSample{}, err
		}
		after := time.Now()
		return clockSample{host: before.Add(after.Sub(before) / 2), db: db}, nil
	}
}

// ClockWatch measures how far the database clock moves against the
// host's monotonic clock from its start. It keeps only its start sample,
// so goroutines may share one.
type ClockWatch struct {
	sample clockSampler
	start  clockSample
}

// StartClockWatch takes the start sample of both clocks through q.
func StartClockWatch(ctx context.Context, q Querier) (*ClockWatch, error) {
	if isNilQuerier(q) {
		return nil, errors.New("testdb: start a clock watch: no querier")
	}
	return startClockWatch(ctx, querierSampler(q))
}

func startClockWatch(ctx context.Context, s clockSampler) (*ClockWatch, error) {
	start, err := s(ctx)
	if err != nil {
		return nil, fmt.Errorf("testdb: start a clock watch: %w", err)
	}
	return &ClockWatch{sample: s, start: start}, nil
}

// Step is how far the database clock moved against the host since the
// start: positive when it ran ahead, negative when it fell behind. Every
// call measures from the start, not from the previous call.
func (w *ClockWatch) Step(ctx context.Context) (time.Duration, error) {
	if w == nil || w.sample == nil {
		return 0, errors.New("testdb: measure the clock step: no clock watch " +
			"(StartClockWatch)")
	}
	now, err := w.sample(ctx)
	if err != nil {
		return 0, fmt.Errorf("testdb: measure the clock step: %w", err)
	}
	return now.db.Sub(w.start.db) - now.host.Sub(w.start.host), nil
}

// Widen is bound widened by the measured step (WidenBound), logged on t.
// A failed sample fails t: a bound is never widened by a zero it guessed.
func (w *ClockWatch) Widen(t testing.TB, bound time.Duration) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), clockSampleTimeout)
	defer cancel()
	step, err := w.Step(ctx)
	if err != nil {
		t.Fatalf("%v", err)
	}
	widened := WidenBound(bound, step)
	t.Logf("database clock moved %v against the host; bound %v widened to %v",
		step, bound, widened)
	return widened
}

// WidenBound is bound plus the size of step: a bound on a database-clock
// interval holds on a slewing clock only by the amount the clock moved.
func WidenBound(bound, step time.Duration) time.Duration {
	return bound + step.Abs()
}
