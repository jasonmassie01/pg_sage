package runway

import (
	"math"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sample is one point of one runway series. Counter is the series'
// monotonic counter (NaN for a level series); Limit is NaN when unknown.
type Sample struct {
	Kind    string
	Subject string
	Epoch   string
	Value   float64
	Counter float64
	Limit   float64
}

// Snapshot is one tick's decoded probe results. A nil pointer or a false
// flag is a probe that could not be read.
type Snapshot struct {
	XID         *probes.XIDRunway
	Tables      []probes.WraparoundTable
	TablesOK    bool
	WAL         *probes.WALRunway
	Dir         *probes.WALDirectory
	Slots       []probes.Slot
	SlotsOK     bool
	Sequences   []probes.SequenceRunway
	SequencesOK bool
}

// BuildSamples turns a snapshot into samples. Only known numbers are
// sampled: an unknown counter, an unreadable database or WAL directory
// produce no sample, never a zero.
func BuildSamples(s Snapshot, opts Options) []Sample {
	nan := math.NaN()
	var out []Sample
	add := func(kind, subject string, value, counter, limit float64) {
		out = append(out, Sample{Kind: kind, Subject: subject, Value: value,
			Counter: counter, Limit: limit})
	}
	if x := s.XID; x != nil {
		if probes.Known(x.NextXID) && probes.Known(x.ClusterXIDAge) {
			add(probes.RunwayXID, probes.SubjectCluster, x.ClusterXIDAge, x.NextXID,
				WraparoundWarnAge)
		}
		if probes.Known(x.MXIDCounter) && probes.Known(x.ClusterMXIDAge) {
			add(probes.RunwayMXID, probes.SubjectCluster, x.ClusterMXIDAge, x.MXIDCounter,
				WraparoundWarnAge)
		}
	}
	if w := s.WAL; w != nil {
		if probes.Known(w.PositionBytes) {
			add(probes.RunwayWALPosition, probes.SubjectCluster, w.PositionBytes,
				w.PositionBytes, nan)
		}
		if probes.Known(w.DatabaseBytes) && w.UnreadableDatabases == 0 {
			add(probes.RunwayDatabaseBytes, probes.SubjectCluster, w.DatabaseBytes, nan, nan)
			if s.Dir != nil && probes.Known(s.Dir.Bytes) {
				add(probes.RunwayDiskUsed, probes.SubjectCluster, w.DatabaseBytes+s.Dir.Bytes,
					nan, capacity(opts))
			}
		}
	}
	for _, sl := range s.Slots {
		if s.SlotsOK && probes.Known(sl.RetainedBytes) {
			add(probes.RunwayWALSlot, sl.Name, sl.RetainedBytes, nan, slotLimit(s.WAL, opts))
		}
	}
	for _, sq := range s.Sequences {
		if s.SequencesOK && !sq.Cycle && probes.Known(sq.LastValue) &&
			probes.Known(sq.Limit) {
			add(probes.RunwaySequence, sq.Sequence, sq.LastValue, sq.LastValue, sq.Limit)
		}
	}
	return out
}

func capacity(opts Options) float64 {
	if opts.DiskCapacityBytes > 0 {
		return opts.DiskCapacityBytes
	}
	return math.NaN()
}

// slotLimit is a bounded max_slot_wal_keep_size (the slot is invalidated
// there), else the WAL custodian's retained-WAL ceiling.
func slotLimit(w *probes.WALRunway, opts Options) float64 {
	if w != nil && w.SlotKeepBounded() {
		return w.MaxSlotWALKeepSize
	}
	if opts.WALRetainedLimitBytes > 0 {
		return opts.WALRetainedLimitBytes
	}
	return math.NaN()
}

type seriesKey struct{ kind, subject string }

type lastPoint struct {
	epoch   string
	counter float64
}

func epochAt(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// assignEpochs stamps each sample with its series' epoch: a new series,
// or a counter that fell (a reset, a restarted or recreated object),
// starts a new one. last is advanced to the new points.
func assignEpochs(ss []Sample, last map[seriesKey]lastPoint, now time.Time) []Sample {
	out := make([]Sample, 0, len(ss))
	for _, s := range ss {
		k := seriesKey{s.Kind, s.Subject}
		p, seen := last[k]
		switch {
		case !seen:
			s.Epoch = epochAt(now)
		case probes.Known(s.Counter) && probes.Known(p.counter) && s.Counter < p.counter:
			s.Epoch = epochAt(now)
		default:
			s.Epoch = p.epoch
		}
		last[k] = lastPoint{epoch: s.Epoch, counter: s.Counter}
		out = append(out, s)
	}
	return out
}
