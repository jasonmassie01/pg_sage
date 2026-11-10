package selfmonitor

import "time"

// ProbeLockTimeout is the lock_timeout of pg_sage's read-only probes of
// the monitored database: the SRE probe catalog's default and the
// on-demand EXPLAIN (rca-05). A probe queued behind an ACCESS EXCLUSIVE
// request (an ALTER TABLE) holds its other locks and lengthens the lock
// queue for every session behind it, so it gives up quickly instead.
const ProbeLockTimeout = 100 * time.Millisecond
