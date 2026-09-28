package verify

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// IO evidence sources, recorded with every rate so a baseline never mixes
// counters with different meanings.
const (
	IOSourcePGStatIO       = "pg_stat_io"
	IOSourcePGStatDatabase = "pg_stat_database+pg_stat_bgwriter"
)

var (
	// ErrCounterReset reports that statistics were reset (or became
	// incomparable) inside the interval. Such a delta says nothing about
	// load, so it must never be read as a quiet period.
	ErrCounterReset = errors.New("IO statistics were reset during the sampling interval")
	// ErrSampleTooSoon reports an interval under MinIOSampleInterval. The
	// previous counters stay in place so a concurrent sampler cannot shrink
	// the measured interval.
	ErrSampleTooSoon = errors.New("IO sample interval is shorter than 1 second")
	// ErrSampleInterval reports an interval over MaxIOSampleInterval.
	ErrSampleInterval = errors.New("IO sample interval is longer than 120 seconds")
)

const (
	MinIOSampleInterval = time.Second
	MaxIOSampleInterval = 120 * time.Second
)

// IOCounterPart is one cumulative counter row, such as one pg_stat_io
// backend/object/context row or one pg_stat_database row. Reset carries the
// row's stats_reset so a reset is detected even after the counter regrows.
type IOCounterPart struct {
	Bytes float64
	Reset string
}

// IOCounters is one reading of the cumulative pg-side IO counters.
type IOCounters struct {
	At        time.Time
	Source    string
	DataParts map[string]IOCounterPart
	WAL       IOCounterPart
}

// IORate is the IO throughput measured between two counter readings.
type IORate struct {
	At              time.Time
	Interval        time.Duration
	DataBytesPerSec float64
	WALBytesPerSec  float64
	Source          string
}

// DeriveIORate turns two cumulative readings into a rate. Parts present in
// only one reading (a database created or dropped in between) are ignored;
// a decreased counter or changed reset time on any shared part is a reset.
func DeriveIORate(previous, current IOCounters) (IORate, error) {
	interval := current.At.Sub(previous.At)
	if interval < MinIOSampleInterval {
		return IORate{}, ErrSampleTooSoon
	}
	if interval > MaxIOSampleInterval {
		return IORate{}, ErrSampleInterval
	}
	if previous.Source == "" || previous.Source != current.Source {
		return IORate{}, fmt.Errorf("%w: counter source changed from %q to %q",
			ErrCounterReset, previous.Source, current.Source)
	}
	dataBytes, err := dataDelta(previous.DataParts, current.DataParts)
	if err != nil {
		return IORate{}, err
	}
	walBytes, err := partDelta("wal", previous.WAL, current.WAL)
	if err != nil {
		return IORate{}, err
	}
	seconds := interval.Seconds()
	return IORate{
		At: current.At, Interval: interval, Source: current.Source,
		DataBytesPerSec: dataBytes / seconds, WALBytesPerSec: walBytes / seconds,
	}, nil
}

func dataDelta(previous, current map[string]IOCounterPart) (float64, error) {
	total, shared := 0.0, 0
	for key, now := range current {
		before, ok := previous[key]
		if !ok {
			continue
		}
		delta, err := partDelta(key, before, now)
		if err != nil {
			return 0, err
		}
		total += delta
		shared++
	}
	if shared == 0 {
		return 0, fmt.Errorf("%w: no comparable data-IO counters", ErrCounterReset)
	}
	return total, nil
}

func partDelta(key string, before, now IOCounterPart) (float64, error) {
	delta := now.Bytes - before.Bytes
	if before.Reset != now.Reset || delta < 0 || math.IsNaN(delta) || math.IsInf(delta, 0) {
		return 0, fmt.Errorf("%w: %s", ErrCounterReset, key)
	}
	return delta, nil
}
