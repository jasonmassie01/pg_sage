package runway

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// sequenceReading is the last sequence_runway reading: when it was
// attempted, what it read (when it succeeded) and the coverage last
// logged.
type sequenceReading struct {
	attempted bool
	at        time.Time
	ok        bool
	list      []probes.SequenceRunway
	logged    probes.SequenceCoverage
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
	res := m.runner.RunBackground(ctx, probes.SequenceRunwayProbe, probes.Args{})
	list, err := probes.Sequences(res)
	m.seq.attempted, m.seq.at = true, now
	m.seq.ok, m.seq.list = err == nil, list
	if err != nil {
		m.seq.list = nil
		return fmt.Errorf("%s: %w", probes.SequenceRunwayProbe, err)
	}
	s.Sequences, s.SequencesOK, s.SequencesFresh = list, true, true
	m.noteCoverage(res)
	return nil
}

// noteCoverage logs the reading's coverage when it changed: a scan cap
// that left sequences unread is a warning, a truncated listing is
// informational.
func (m *Monitor) noteCoverage(res probes.Result) {
	c, err := probes.SequenceCoverageOf(res)
	if err != nil {
		m.logFn("WARN", "runway: db %q: sequence coverage unreadable: %v",
			m.opts.Database, err)
		return
	}
	if c == m.seq.logged {
		return
	}
	m.seq.logged = c
	switch {
	case c.ScanCapped():
		m.logFn("WARN", "runway: db %q: sequence scan cap reached: read %d of %d "+
			"sequences (smallest capacity first); %d used, the %d closest to their limit "+
			"sampled", m.opts.Database, c.Scanned, c.Total, c.Used, c.Reported)
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
