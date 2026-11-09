package runway

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// maxSequencesReported is how many sequences a reading lists by their
// nearness to their limit (sequence_runway's own top per statement); the
// kept sequences (sequenceKeep) are listed beside them.
const maxSequencesReported = probes.SequenceTopN

// sequenceReading is the last sequence_runway reading: when it was
// attempted, what it read (when it succeeded), the coverage last logged,
// and how the catalog is sliced: slices statements per pass, each able to
// read perRead sequences (the probe's cap, or less under a small lock
// table).
type sequenceReading struct {
	attempted bool
	at        time.Time
	ok        bool
	list      []probes.SequenceRunway
	logged    probes.SequenceCoverage
	slices    int
	perRead   int64
}

// readSequences fills the snapshot's sequences. They are read through
// the probe's background budget when due (every SequenceInterval; every
// tick when it is 0); between reads the last good reading is evaluated
// but not sampled again (SequencesFresh is false). A failed read is
// reported once and not retried until the next due time, and nothing is
// evaluated from an earlier reading meanwhile.
func (m *Monitor) readSequences(ctx context.Context, s *Snapshot) error {
	m.seqMu.Lock()
	defer m.seqMu.Unlock()
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	due := !m.seq.attempted || m.opts.SequenceInterval <= 0 ||
		!now.Before(m.seq.at.Add(m.opts.SequenceInterval))
	if !due {
		s.Sequences, s.SequencesOK = m.seq.list, m.seq.ok
		return nil
	}
	list, cov, err := m.readSequencePass(ctx)
	m.seq.attempted, m.seq.at = true, now
	m.seq.ok, m.seq.list = err == nil, list
	if err != nil {
		m.seq.list = nil
		return fmt.Errorf("%s: %w", probes.SequenceRunwayProbe, err)
	}
	s.Sequences, s.SequencesOK, s.SequencesFresh = list, true, true
	m.noteCoverage(cov)
	return nil
}

// readSequencePass reads every sequence of the catalog (performance gate
// offender 5, v1.8.3): one statement reads at most the probe's scan cap,
// so a larger catalog is read in slices, one statement and transaction
// each. The slice count comes from the previous pass; a pass that found
// a statement capped is read again at once in enough slices.
func (m *Monitor) readSequencePass(ctx context.Context) ([]probes.SequenceRunway,
	probes.SequenceCoverage, error) {
	if m.seq.perRead <= 0 {
		m.seq.perRead = probes.SequenceScanCap
	}
	slices := max(m.seq.slices, 1)
	keep := m.sequenceKeep()
	list, cov, perRead, err := m.readSlices(ctx, slices, keep)
	if err != nil {
		return nil, cov, err
	}
	if perRead > 0 {
		m.seq.perRead = min(m.seq.perRead, perRead)
	}
	if want := sliceCount(cov.Total, m.seq.perRead); cov.ScanCapped() && want > slices {
		slices = want
		if list, cov, perRead, err = m.readSlices(ctx, slices, keep); err != nil {
			return nil, cov, err
		}
		if perRead > 0 {
			m.seq.perRead = min(m.seq.perRead, perRead)
		}
	}
	m.seq.slices = max(slices, sliceCount(cov.Total, m.seq.perRead))
	if !cov.ScanCapped() {
		m.seq.slices = sliceCount(cov.Total, m.seq.perRead)
	}
	return list, cov, nil
}

// sliceCount is how many slices read total sequences at perRead each,
// with a quarter of headroom for uneven slices.
func sliceCount(total, perRead int64) int {
	if perRead <= 0 || total <= perRead {
		return 1
	}
	per := max(perRead*3/4, 1)
	return int(min((total+per-1)/per, probes.MaxSequenceSlices))
}

// readSlices reads the catalog in slices statements and merges them: the
// sequences nearest their limit first, then the kept ones past them, the
// coverage summed. perRead is the most a capped statement read (0 when
// none was capped).
func (m *Monitor) readSlices(ctx context.Context, slices int,
	keep []string) ([]probes.SequenceRunway, probes.SequenceCoverage, int64, error) {
	var list []probes.SequenceRunway
	var cov probes.SequenceCoverage
	var perRead int64
	for i := 0; i < slices; i++ {
		args := probes.Args{Keep: keep}
		if slices > 1 {
			args.Slices, args.Slice = slices, i
		}
		res := m.runner.RunBackground(ctx, probes.SequenceRunwayProbe, args)
		ss, err := probes.Sequences(res)
		if err != nil {
			return nil, cov, 0, err
		}
		c, err := probes.SequenceCoverageOf(res)
		if err != nil {
			return nil, cov, 0, err
		}
		if c.ScanCapped() {
			perRead = max(perRead, c.Scanned)
		}
		list = append(list, ss...)
		cov.Total, cov.Scanned = cov.Total+c.Total, cov.Scanned+c.Scanned
		cov.Used, cov.Unreadable = cov.Used+c.Used, cov.Unreadable+c.Unreadable
		cov.Truncated = cov.Truncated || c.Truncated
	}
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].Fraction > list[j].Fraction ||
			(list[i].Fraction == list[j].Fraction && list[i].Sequence < list[j].Sequence)
	})
	list, cut := topAndKept(list, keep)
	cov.Truncated = cov.Truncated || cut
	cov.Reported = int64(len(list))
	return list, cov, perRead, nil
}

// topAndKept keeps the first maxSequencesReported of a sorted list and,
// past them, the kept sequences; cut reports a sequence left out.
func topAndKept(list []probes.SequenceRunway, keep []string) ([]probes.SequenceRunway,
	bool) {
	if len(list) <= maxSequencesReported {
		return list, false
	}
	kept := make(map[string]bool, len(keep))
	for _, k := range keep {
		kept[k] = true
	}
	out := list[:maxSequencesReported:maxSequencesReported]
	for _, s := range list[maxSequencesReported:] {
		if kept[s.Sequence] {
			out = append(out, s)
		}
	}
	return out, len(out) < len(list)
}

// noteSequenceSeries remembers the sequences with samples in the
// lookback (the sequence series of a runway_trends read), at most
// probes.MaxSequenceKeep, soonest to their limit first: the next reading
// lists them whatever their rank, so a sampled sequence keeps its series
// (forecast-10).
func (m *Monitor) noteSequenceSeries(set probes.RunwayTrendSet) {
	var ts []probes.RunwayTrend
	for _, tr := range set.Trends {
		if tr.Kind == probes.RunwaySequence && tr.Subject != "" {
			ts = append(ts, tr)
		}
	}
	soon := func(tr probes.RunwayTrend) float64 {
		if s := tr.SecondsToLimit(); !math.IsNaN(s) {
			return s
		}
		return math.Inf(1)
	}
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := soon(ts[i]), soon(ts[j])
		return a < b || (a == b && ts[i].Subject < ts[j].Subject)
	})
	keep := make([]string, 0, min(len(ts), probes.MaxSequenceKeep))
	for _, tr := range ts[:min(len(ts), probes.MaxSequenceKeep)] {
		keep = append(keep, tr.Subject)
	}
	m.keepMu.Lock()
	defer m.keepMu.Unlock()
	if !set.Complete(probes.RunwaySequence) { // a cut read: forget no series
		keep = mergeKeep(keep, m.seqKeep)
	}
	m.seqKeep = keep
}

// mergeKeep is fresh then the earlier names not in it, capped at
// probes.MaxSequenceKeep.
func mergeKeep(fresh, earlier []string) []string {
	seen := make(map[string]bool, len(fresh))
	for _, k := range fresh {
		seen[k] = true
	}
	for _, k := range earlier {
		if len(fresh) >= probes.MaxSequenceKeep {
			break
		}
		if !seen[k] {
			fresh = append(fresh, k)
		}
	}
	return fresh
}

// sequenceKeep is a copy of the sequences the next reading keeps.
func (m *Monitor) sequenceKeep() []string {
	m.keepMu.Lock()
	defer m.keepMu.Unlock()
	return append([]string(nil), m.seqKeep...)
}

// noteCoverage logs the reading's coverage when it changed: a scan cap
// that left sequences unread is a warning, a truncated listing is
// informational.
func (m *Monitor) noteCoverage(c probes.SequenceCoverage) {
	if c == m.seq.logged {
		return
	}
	m.seq.logged = c
	switch {
	case c.ScanCapped():
		m.logFn("WARN", "runway: db %q: sequence scan cap reached: read %d of %d "+
			"sequences (smallest capacity first); %d used, the %d closest to their limit "+
			"sampled; each read takes a lock, so one statement reads at most a quarter of "+
			"the lock table (raise max_locks_per_transaction to read more)",
			m.opts.Database, c.Scanned, c.Total, c.Used, c.Reported)
	case c.Truncated:
		m.logFn("INFO", "runway: db %q: %d of %d sequences used; sampling the %d "+
			"nearest their limit", m.opts.Database, c.Used, c.Total, c.Reported)
	}
	if c.Unreadable > 0 {
		m.logFn("WARN", "runway: db %q: %d sequences are not readable by this role "+
			"(grant SELECT or pg_read_all_data to measure them)", m.opts.Database,
			c.Unreadable)
	}
}
