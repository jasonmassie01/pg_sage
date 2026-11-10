package migration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/rca"
)

// LockQueueSource is one database's latest lock-chain observation
// (analyzer.LockChainObserver): Fresh returns it while it is at most two
// fast-path intervals old, and Nudge asks the fast path to observe soon.
// The detector adds no blocking-pid query of its own (migration-05).
type LockQueueSource interface {
	Fresh() ([]analyzer.LockChain, time.Time, bool)
	Nudge()
}

// lockQueueRule is the rule of a finding opened only because of the queue
// behind a DDL the advisor scored low (plain ADD COLUMN, for one).
const lockQueueRule = "ddl_lock_queue"

const lockQueueNotObserved = "lock queue not observed"

// activeDDL is one in-flight DDL backend from pg_stat_activity.
type activeDDL struct {
	PID           int
	Query         string // trimmed; what is analysed
	RawQuery      string // as pg_stat_activity shows it (hashed for identity)
	WaitEventType string
	Role          string
	BackendStart  time.Time
	QueryStart    time.Time
}

func (a activeDDL) waitingOnLock() bool { return a.WaitEventType == "Lock" }

// trackedDDL is a DDL the detector has analysed; inc is nil when the
// advisor found no risk.
type trackedDDL struct {
	ddl  activeDDL
	inc  *rca.Incident
	risk *DDLRisk // the advisor's assessment; nil for a queue-only finding
}

// lockQueueVerdict is the latest observation of the queue behind a DDL.
type lockQueueVerdict struct {
	Observed   bool
	ObservedAt time.Time
	Waiters    int
	RootPID    int
	OldestWait time.Time
	Escalate   bool
}

// lockQueueVerdict correlates the DDL with the latest fresh observation:
// the DDL as a blocked pid with DirectWaiters at or above the threshold
// escalates.
func (d *Detector) lockQueueVerdict(a activeDDL) lockQueueVerdict {
	if d.lockQueue == nil {
		return lockQueueVerdict{}
	}
	chains, at, ok := d.lockQueue.Fresh()
	if !ok {
		return lockQueueVerdict{}
	}
	v := lockQueueVerdict{Observed: true, ObservedAt: at}
	for _, c := range chains {
		if !containsPID(c.BlockedPIDs, a.PID) {
			continue
		}
		if n := c.DirectWaiters[a.PID]; n >= v.Waiters {
			v.Waiters, v.RootPID, v.OldestWait = n, c.RootBlockerPID, c.OldestWaitSince
		}
	}
	v.Escalate = v.Waiters >= d.cfg.QueueWaiters()
	return v
}

// QueuedDDLPID implements analyzer.QueuedDDL: a chain is the queue behind
// a tracked DDL when the DDL, waiting on a lock, is one of its blocked
// pids with at least migration.queue_waiters_threshold direct waiters.
func (d *Detector) QueuedDDLPID(c analyzer.LockChain) (int, bool) {
	if d == nil {
		return 0, false
	}
	threshold := d.cfg.QueueWaiters()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, pid := range c.BlockedPIDs {
		t, ok := d.tracked[pid]
		if ok && t.ddl.waitingOnLock() && c.DirectWaiters[pid] >= threshold {
			return pid, true
		}
	}
	return 0, false
}

func containsPID(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}

// queueIncident opens a finding for a DDL the advisor scored low once a
// queue builds behind it, on the DDL's table when it can be classified.
// Its own severity is warning: applyLockQueue raises it to critical only
// while the queue is at the threshold.
func (d *Detector) queueIncident(a activeDDL) *rca.Incident {
	object := d.queuedDDLObject(a.Query)
	inc := &rca.Incident{
		DetectedAt: time.Now(), Severity: "warning", Source: "schema_advisor",
		RootCause: fmt.Sprintf("DDL pid %d is queued for its lock with sessions "+
			"waiting behind it", a.PID),
		SignalIDs: []string{lockQueueRule}, ActionRisk: "lock_queue",
		DatabaseName: d.advisor.dbName, Confidence: 1,
	}
	if object != "" {
		inc.AffectedObjects = []string{object}
	}
	return inc
}

// queuedDDLObject names the DDL's table: from a matched rule, else from
// the ALTER TABLE or ON clause; "" when neither names one.
func (d *Detector) queuedDDLObject(sql string) string {
	clean := sanitizeDDL(sql)
	for _, c := range d.advisor.classifier.Classify(clean, d.advisor.pgVersion) {
		if c.TableName != "" {
			return qualifiedName(c.SchemaName, c.TableName)
		}
	}
	var c DDLClassification
	fillTableFromAlter(collapseWhitespace(clean), &c)
	if c.TableName == "" {
		fillTableFromOnClause(collapseWhitespace(clean), &c)
	}
	if c.TableName == "" {
		return ""
	}
	return qualifiedName(c.SchemaName, c.TableName)
}

// applyLockQueue records the verdict on the finding. An escalated queue
// makes the finding critical with the DDL's backend identity and
// approval-only cancel guidance; RecommendedSQL never carries the cancel.
func applyLockQueue(f *MigrationSafetyFinding, a activeDDL, v lockQueueVerdict) {
	if !v.Observed {
		f.Detail["lock_queue"] = lockQueueNotObserved
		return
	}
	f.Detail["lock_queue"] = "observed"
	f.Detail["lock_queue_waiters"] = v.Waiters
	f.Detail["lock_queue_observed_at"] = v.ObservedAt
	if !v.Escalate {
		return
	}
	f.Severity = "critical"
	f.Detail["lock_queue_escalated"] = true
	f.Detail["lock_queue_root_pid"] = v.RootPID
	f.Detail["lock_queue_oldest_wait"] = v.OldestWait
	addDDLBackendEvidence(f.Detail, a)
	cancel := fmt.Sprintf("SELECT pg_cancel_backend(%d)", a.PID)
	f.Detail["cancel_guidance_sql"] = cancel
	f.Recommendation = fmt.Sprintf("DDL pid %d waits for its lock behind pid %d "+
		"with %d sessions queued behind it. Requires approval: cancel DDL pid %d "+
		"(%s), then retry with SET lock_timeout = '5s'. On managed services "+
		"pg_cancel_backend needs the pg_signal_backend role.",
		a.PID, v.RootPID, v.Waiters, a.PID, cancel)
}

// addDDLBackendEvidence records the DDL backend's identity with the keys
// the lock-chain evidence uses (pid, backend_start, query_start, query).
// The query is kept sanitized; query_sha hashes the first 200 characters
// of the raw text, which a guarded cancel compares with
// left(query, 200), so no literal is stored.
func addDDLBackendEvidence(detail map[string]any, a activeDDL) {
	detail["pid"] = a.PID
	detail["backend_start"] = a.BackendStart
	detail["query_start"] = a.QueryStart
	detail["state"] = "active"
	detail["query"] = leftRunes(sanitizeDDL(a.Query), 200)
	raw := a.RawQuery
	if raw == "" {
		raw = a.Query
	}
	sum := sha256.Sum256([]byte(leftRunes(raw, 200)))
	detail["query_sha"] = hex.EncodeToString(sum[:8])
}

// leftRunes mirrors SQL LEFT(s, n): the first n characters.
func leftRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
