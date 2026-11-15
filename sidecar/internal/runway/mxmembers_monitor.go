package runway

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// MXMemberResult is what one tick learned of the member space: nil when
// this monitor does not report the cluster's series (another database of
// the cluster does). Recorded says a sample was stored, Opened that the
// finding was opened or refreshed and Resolved that an open one cleared.
type MXMemberResult struct {
	Estimate MXMemberEstimate
	Runway   MXMemberRunway
	Recorded bool
	Opened   bool
	Resolved bool
}

const (
	mxInsertSQL = `/* pg_sage */ INSERT INTO sage.multixact_member_samples
		(database, checkpoint_at, next_mxid, next_offset, oldest_mxid, live_multixacts,
		 sampled, members_est, stderr)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	// mxPreviousSQL is the oldest sample of the lookback taken at an
	// earlier checkpoint: the rate's starting point.
	mxPreviousSQL = `/* pg_sage */ SELECT next_offset, checkpoint_at
		FROM sage.multixact_member_samples
		WHERE database = $1 AND sampled_at > now() - make_interval(secs => $2)
		  AND checkpoint_at < $3
		ORDER BY sampled_at LIMIT 1`
	mxPruneSQL = `/* pg_sage */ DELETE FROM sage.multixact_member_samples
		WHERE id = ANY (ARRAY(SELECT id FROM sage.multixact_member_samples
		             WHERE sampled_at < now() - make_interval(secs => $1)
		             LIMIT $2))`
)

// tickMXMembers reads the member space at most once per
// Options.MXMemberInterval (the last result in between), records a
// measured reading and opens or resolves the finding. An unknown reading
// records nothing and leaves an open finding as it is: what cannot be
// measured is never resolved. Ticks of one monitor are serialized.
func (m *Monitor) tickMXMembers(ctx context.Context, scope clusterScope) (*MXMemberResult,
	error) {
	if !scope.reports() {
		return nil, nil
	}
	m.mxMu.Lock()
	defer m.mxMu.Unlock()
	now := m.now()
	if m.mxLast != nil && m.opts.MXMemberInterval > 0 &&
		now.Sub(m.mxAt) < m.opts.MXMemberInterval {
		return m.mxLast, nil
	}
	e, err := m.mxRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("multixact member space: %w", err)
	}
	m.noteMXStatus(e)
	res := &MXMemberResult{Estimate: e}
	if e.Status == MXMeasured {
		if err := m.recordMXMembers(ctx, scope, res); err != nil {
			return res, fmt.Errorf("multixact member space: %w", err)
		}
	}
	m.mxAt, m.mxLast = now, res
	return res, nil
}

// recordMXMembers projects a measured reading from the lookback's
// earlier checkpoint, stores it, prunes past the retention and reports
// the finding.
func (m *Monitor) recordMXMembers(ctx context.Context, scope clusterScope,
	res *MXMemberResult) error {
	e := res.Estimate
	prev, ok, err := m.previousMXSample(ctx, e)
	if err != nil {
		return err
	}
	rate, rateOK := 0.0, false
	if ok {
		rate, rateOK = memberRate(prev, e.MXControl)
	}
	res.Runway = mxRunway(e, rate, rateOK, m.mxLimits)
	if _, err := m.pool.Exec(ctx, mxInsertSQL, m.opts.Database, e.CheckpointAt,
		int64(e.NextMXID), int64(e.NextOffset), int64(e.OldestMXID), int64(e.Live),
		e.Sampled, e.Members, e.StdErr); err != nil {
		return fmt.Errorf("record the sample: %w", err)
	}
	res.Recorded = true
	if _, err := m.pool.Exec(ctx, mxPruneSQL, MXMemberRetention.Seconds(),
		pruneBatch); err != nil {
		return fmt.Errorf("prune the samples: %w", err)
	}
	return m.reportMXMembers(ctx, scope, res)
}

func (m *Monitor) previousMXSample(ctx context.Context, e MXMemberEstimate) (MXControl,
	bool, error) {
	var offset int64
	var c MXControl
	err := m.pool.QueryRow(ctx, mxPreviousSQL, m.opts.Database, m.opts.Lookback.Seconds(),
		e.CheckpointAt).Scan(&offset, &c.CheckpointAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MXControl{}, false, nil
	}
	if err != nil {
		return MXControl{}, false, fmt.Errorf("read the previous sample: %w", err)
	}
	c.NextOffset = uint32(offset)
	return c, true, nil
}

// reportMXMembers opens (or refreshes) the finding of a runway inside
// its horizon and resolves an open one once the runway is measured clear.
// A runway without a rate (the first sample, a reset) is unknown: an open
// finding is held, not resolved.
func (m *Monitor) reportMXMembers(ctx context.Context, scope clusterScope,
	res *MXMemberResult) error {
	if res.Runway.Severity == "" && math.IsNaN(res.Runway.SecondsToSafe) {
		return nil
	}
	if res.Runway.Severity != "" {
		f := mxFinding(res.Estimate, res.Runway, m.mxLimits, scope)
		if _, err := analyzer.UpsertFindingsWithResult(ctx, m.pool,
			[]analyzer.Finding{f}); err != nil {
			return fmt.Errorf("record the finding: %w", err)
		}
		res.Opened = true
		return nil
	}
	cleared, err := analyzer.ResolveClearedFindings(ctx, m.pool, map[string]bool{},
		CategoryMXMembers)
	if err != nil {
		return fmt.Errorf("resolve the cleared finding: %w", err)
	}
	res.Resolved = len(cleared) > 0
	return nil
}

// noteMXStatus logs a change of the reading's status once: unknown names
// the grant that would make it measurable.
func (m *Monitor) noteMXStatus(e MXMemberEstimate) {
	if e.Status == m.mxStatus {
		return
	}
	if e.Status == MXUnknown {
		m.logFn("WARN", "runway: db %q: multixact member space is unknown: %s; to "+
			"measure it: %s", m.opts.Database, e.Reason, e.Grant)
	} else if m.mxStatus == MXUnknown {
		m.logFn("INFO", "runway: db %q: multixact member space is measured again",
			m.opts.Database)
	}
	m.mxStatus = e.Status
}
