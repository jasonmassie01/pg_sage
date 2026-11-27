package advisor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

const (
	// walSkipRatio is the requested share of checkpoints below which the
	// WAL sub-advisor does not ask the model: checkpoints keep up.
	walSkipRatio = 0.20
	// walFloorFactor sizes the lowest max_wal_size: one and a half
	// checkpoint_timeouts' worth of the window's WAL.
	walFloorFactor = 1.5
	// walCeilingFactor caps an executable raise at 4 x the floor.
	walCeilingFactor = 4.0
	// walMinTimeouts is the shortest window the facts are measured over, in
	// checkpoint_timeouts: fewer cannot show the timed checkpoints.
	walMinTimeouts = 3
	// defaultCheckpointTimeout is PostgreSQL's, for an unreadable setting.
	defaultCheckpointTimeout = 5 * time.Minute
	mib                      = 1 << 20
)

// walWindowFacts is what the advisor window says about checkpoints and WAL
// (advisors-05). A field that is not known is nil, never zero: a reset of
// its statistics between the rows, a counter that fell, or a missing row.
type walWindowFacts struct {
	window, measured  time.Duration // the advisor window; the measured span
	startAt           time.Time
	known             bool   // a window-start row spans walMinTimeouts
	unknownWhy        string // why known is false
	checkpointTimeout time.Duration
	// reqDelta and timedDelta are the window's checkpoints (0 when unknown).
	reqDelta, timedDelta int64
	requestedRatio       *float64 // reqDelta / (reqDelta + timedDelta)
	walPerHour           *float64 // bytes of WAL per hour
	walPerTimeout        *float64 // bytes of WAL per checkpoint_timeout
}

// walFacts reads the system row nearest the start of the advisor window
// through the history store and measures the window against snap. A read
// error is returned: the advisor never recommends from one snapshot.
func walFacts(ctx context.Context, hist windowHistory, snap *collector.Snapshot,
	window time.Duration) (walWindowFacts, error) {
	if snap == nil {
		return walWindowFacts{}, errors.New("wal facts: no snapshot to measure")
	}
	f := walWindowFacts{window: window, checkpointTimeout: checkpointTimeoutOf(snap)}
	if hist == nil {
		f.unknownWhy = "no snapshot history is available"
		return f, nil
	}
	rctx, cancel := context.WithTimeout(ctx, windowHistoryTimeout)
	defer cancel()
	start, found, err := hist.readSystemStart(rctx, window)
	if err != nil {
		return f, fmt.Errorf("reading the system history of the last %s: %w", window, err)
	}
	if !found {
		f.unknownWhy = fmt.Sprintf("no system snapshot in the last %s", window)
		return f, nil
	}
	return measureWALWindow(f, start, snap), nil
}

// measureWALWindow computes the facts between the window-start row and snap.
func measureWALWindow(f walWindowFacts, start walStart, snap *collector.Snapshot,
) walWindowFacts {
	f.startAt, f.measured = start.at, snap.CollectedAt.Sub(start.at)
	if least := walMinTimeouts * f.checkpointTimeout; f.measured < least {
		f.unknownWhy = fmt.Sprintf("the oldest snapshot in the window is %s old, under "+
			"%d checkpoint_timeouts (%s)", f.measured.Round(time.Second), walMinTimeouts, least)
		return f
	}
	f.known = true
	f.reqDelta, f.timedDelta, f.requestedRatio = requestedShare(start.sys, snap.System)
	if bytes, span, ok := walGenerated(start.sys, snap.System, f.measured); ok {
		perHour := bytes * time.Hour.Seconds() / span.Seconds()
		perTimeout := bytes * f.checkpointTimeout.Seconds() / span.Seconds()
		f.walPerHour, f.walPerTimeout = &perHour, &perTimeout
	}
	return f
}

// requestedShare is the window's requested and timed checkpoints and the
// requested share; unknown (0, 0, nil) when the checkpoint statistics epoch
// moved, a counter fell, or no checkpoint happened.
func requestedShare(start, cur collector.SystemStats) (int64, int64, *float64) {
	if !sameStatsEpoch(start.CheckpointStatsEpoch, cur.CheckpointStatsEpoch) {
		return 0, 0, nil
	}
	req := cur.CheckpointsRequested - start.CheckpointsRequested
	timed := cur.CheckpointsTimed - start.CheckpointsTimed
	if req < 0 || timed < 0 || req+timed == 0 {
		return 0, 0, nil
	}
	ratio := float64(req) / float64(req+timed)
	return req, timed, &ratio
}

// walGenerated is the WAL written over the window and the span it was
// measured over: pg_stat_wal.wal_bytes under one epoch, else the insert
// positions (legacy start rows, a pg_stat_wal reset).
func walGenerated(start, cur collector.SystemStats, measured time.Duration) (
	float64, time.Duration, bool) {
	if sameStatsEpoch(start.WALStatsEpoch, cur.WALStatsEpoch) && start.WALBytes > 0 &&
		cur.WALBytes >= start.WALBytes {
		return float64(cur.WALBytes - start.WALBytes), measured, true
	}
	span := cur.WALReadAt.Sub(start.WALReadAt)
	if start.WALLSN > 0 && cur.WALLSN >= start.WALLSN && span > 0 {
		return float64(cur.WALLSN - start.WALLSN), span, true
	}
	return 0, 0, false
}

// checkpointTimeoutOf is the snapshot's checkpoint_timeout.
func checkpointTimeoutOf(snap *collector.Snapshot) time.Duration {
	if snap.ConfigData == nil {
		return defaultCheckpointTimeout
	}
	for _, s := range snap.ConfigData.PGSettings {
		if s.Name != "checkpoint_timeout" {
			continue
		}
		ms, err := pgconf.ParseValue(s.Setting, pgconf.GUCDoc{Unit: "ms", BaseUnit: s.Unit})
		if err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	return defaultCheckpointTimeout
}

// walFloor is the lowest max_wal_size pg_sage accepts: walFloorFactor x
// the WAL per checkpoint_timeout, rounded up to a megabyte and clamped to
// the documented range. Unknown without a measured WAL rate.
type walFloor struct {
	bytes float64
	known bool
}

func walSizeFloor(f walWindowFacts) walFloor {
	if !f.known || f.walPerTimeout == nil {
		return walFloor{}
	}
	doc := pgconf.Docs["max_wal_size"]
	v := math.Ceil(walFloorFactor**f.walPerTimeout/mib) * mib
	return walFloor{bytes: math.Min(math.Max(v, doc.SafeMin), doc.SafeMax), known: true}
}

// walFactLines are the window's facts for the prompt.
func walFactLines(f walWindowFacts, fl walFloor) string {
	if !f.known {
		return fmt.Sprintf("Advisor window: %s, unknown (%s)\n"+
			"Checkpoints in the window: unknown\nWAL generated: unknown\n"+
			"Lowest max_wal_size: unknown (do not recommend a max_wal_size value)\n"+
			"Highest max_wal_size: unknown", f.window, f.unknownWhy)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Advisor window: %s, measured over %s since the snapshot of %s\n",
		f.window, f.measured.Round(time.Second), f.startAt.UTC().Format("2006-01-02 15:04 MST"))
	if f.requestedRatio != nil {
		fmt.Fprintf(&b, "Checkpoints in the window: %d requested, %d timed (requested "+
			"share %.1f%%)\n", f.reqDelta, f.timedDelta, *f.requestedRatio*100)
	} else {
		b.WriteString("Checkpoints in the window: unknown (none, or the checkpoint " +
			"statistics were reset)\n")
	}
	if f.walPerHour != nil {
		fmt.Fprintf(&b, "WAL generated: %s/h, %s per checkpoint_timeout (%s)\n",
			walSize(*f.walPerHour), walSize(*f.walPerTimeout), f.checkpointTimeout)
	} else {
		b.WriteString("WAL generated: unknown (the WAL statistics were reset)\n")
	}
	if !fl.known {
		b.WriteString("Lowest max_wal_size: unknown (do not recommend a max_wal_size " +
			"value)\nHighest max_wal_size: unknown")
		return b.String()
	}
	doc := pgconf.Docs["max_wal_size"]
	fmt.Fprintf(&b, "Lowest max_wal_size: %.0fMB (%.1f x WAL per checkpoint_timeout, "+
		"within %s)\nHighest max_wal_size: %.0fMB (%.0f x the lowest)",
		fl.bytes/mib, walFloorFactor, pgconf.RangeString(doc),
		walCeilingFactor*fl.bytes/mib, walCeilingFactor)
	return b.String()
}

// walSize renders bytes as GB at or above a gigabyte, MB below.
func walSize(b float64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GB", b/(1<<30))
	}
	return fmt.Sprintf("%.1f MB", b/mib)
}
