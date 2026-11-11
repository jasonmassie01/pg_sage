package advisor

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// advisors-05 made a WAL finding executable only with the advisor window's
// history behind it. Tests about other concerns (ownership, platforms,
// open findings, defaults) that use the WAL sub-advisor as their vehicle
// get that history from this fixture: the walSnapshots counters an hour
// after walFixtureStart, so 200 of 210 checkpoints were requested and
// 12 GiB of WAL was written (floor 1536MB, ceiling 6144MB, around the
// models' 4GB answers).

// walFixtureCounters are walSnapshots' checkpoint and WAL counters.
var walFixtureCounters = collector.SystemStats{TotalCheckpoints: 400,
	CheckpointsRequested: 300, CheckpointsTimed: 100, CheckpointStatsEpoch: ckptEpoch,
	WALBytes: 22 << 30, WALStatsEpoch: walStatEpoch}

// walFixtureStart is the window-start row of the fixture.
var walFixtureStart = collector.SystemStats{TotalCheckpoints: 190,
	CheckpointsRequested: 100, CheckpointsTimed: 90, CheckpointStatsEpoch: ckptEpoch,
	WALBytes: 10 << 30, WALStatsEpoch: walStatEpoch}

// walFixtureHistory returns the fixture's start row an hour before now (the
// walSnapshots snapshot is taken at time.Now()) and no tables row.
type walFixtureHistory struct{}

func (walFixtureHistory) readSystemStart(context.Context, time.Duration) (walStart, bool,
	error) {
	return walStart{at: time.Now().Add(-time.Hour), sys: walFixtureStart}, true, nil
}

func (walFixtureHistory) readTablesStart(context.Context, time.Duration, []tableKey) (
	tablesStart, bool, error) {
	return tablesStart{}, false, nil
}

// withWALFixtureWindow is a context carrying the fixture's history.
func withWALFixtureWindow(ctx context.Context) context.Context {
	return withAdvisorWindow(ctx, advisorWindow{src: walFixtureHistory{}, window: 24 * time.Hour})
}
