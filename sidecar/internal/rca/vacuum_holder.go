package rca

import (
	"crypto/md5"
	"encoding/hex"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// rca-01: the vacuum_blocked tree names the oldest holder of this
// database's xmin horizon, from the holders the collector reads with
// probes.XminHorizonSQL (the freeze custodian's query). Every open
// transaction holds a virtualxid lock, so a lock row alone is no
// evidence that a session holds the horizon back.

// vacuumRules are the vacuum_blocked tree's thresholds.
type vacuumRules struct {
	// holderFloor is how long a session's transaction must be open
	// before it is named (rca.xmin_holder_min_minutes).
	holderFloor time.Duration
	// preparedFloor is how old a prepared transaction must be before it
	// is named (rca.prepared_xact_min_age_minutes).
	preparedFloor time.Duration
	// idleTerminate is how long a holder must be idle in transaction
	// before pg_terminate_backend is proposed
	// (lock_chain.idle_in_tx_terminate_minutes).
	idleTerminate time.Duration
	// safePatterns are application-name substrings never signalled
	// (lock_chain.safe_patterns), besides protectedApplications.
	safePatterns []string
}

// protectedApplications are never signalled: dumps, base backups,
// restores and pg_sage itself (the freeze custodian's list).
var protectedApplications = []string{"pg_dump", "pg_basebackup", "pg_restore", "pg_sage"}

// vacuumRules reads the floors from the engine's rca section and the
// terminate minutes and safe patterns from lock_chain; zero is a default.
func (e *Engine) vacuumRules(cfg *config.Config) vacuumRules {
	r := vacuumRules{holderFloor: e.cfg.XminHolderFloor(),
		preparedFloor: e.cfg.PreparedXactFloor(),
		idleTerminate: config.DefaultLockChainIdleInTxTerminate * time.Minute}
	if cfg == nil {
		return r
	}
	if m := cfg.Analyzer.LockChain.IdleInTxTerminateMinutes; m > 0 {
		r.idleTerminate = time.Duration(m) * time.Minute
	}
	r.safePatterns = cfg.Analyzer.LockChain.SafePatterns
	return r
}

// protected reports an application pg_sage never signals.
func (r vacuumRules) protected(app string) bool {
	app = strings.ToLower(app)
	for _, list := range [][]string{protectedApplications, r.safePatterns} {
		for _, p := range list {
			if p != "" && strings.Contains(app, strings.ToLower(p)) {
				return true
			}
		}
	}
	return false
}

// xminCandidate is one holder as the tree sees it.
type xminCandidate struct {
	holder probes.XminHolder
	// lock is the session's lock-graph row (state_change, xact_start),
	// nil when the graph does not list it.
	lock *collector.LockInfo
	// complete marks a holder from the full holder read, which also saw
	// slots, standbys and prepared transactions. A candidate rebuilt from
	// lock rows alone may not be the oldest holder, so it is never
	// signalled.
	complete bool
}

// xminCandidates lists the holders oldest first: the collector's holder
// read, or (only when that read is missing: nil) the lock graph's
// sessions with an xmin. An empty read means no holder; the lock graph
// would still list a lazy VACUUM's snapshot, which holds nothing back.
func xminCandidates(curr *collector.Snapshot) []xminCandidate {
	var out []xminCandidate
	if curr.XminHolders != nil {
		for _, h := range curr.XminHolders {
			out = append(out, xminCandidate{holder: h, lock: lockRowOf(curr, h), complete: true})
		}
	} else {
		for i := range curr.Locks {
			if c, ok := lockCandidate(curr, &curr.Locks[i]); ok {
				out = append(out, c)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return holderBefore(out[i], out[j]) })
	return out
}

// holderBefore orders holders by xmin age, oldest first. On a tie (a
// snapshot pinned at another holder's XID has that XID's age) the XID's
// owner comes first: a prepared transaction, then the session whose
// transaction began first. Otherwise the reader of an idle writer's XID
// could be named (and terminated) while the writer still holds it.
func holderBefore(a, b xminCandidate) bool {
	if a.holder.XminAge != b.holder.XminAge {
		return a.holder.XminAge > b.holder.XminAge
	}
	ra, rb := tieRank(a.holder.Kind), tieRank(b.holder.Kind)
	if ra != rb {
		return ra < rb
	}
	return a.holder.XactAgeS > b.holder.XactAgeS
}

func tieRank(kind string) int {
	if kind == probes.HolderPreparedXact {
		return 0
	}
	return 1
}

// lockCandidate rebuilds a session holder from a lock-graph session row.
func lockCandidate(curr *collector.Snapshot, l *collector.LockInfo) (xminCandidate, bool) {
	if l.BackendXminAge == nil || l.XactStart == nil {
		return xminCandidate{}, false
	}
	h := probes.XminHolder{Kind: probes.HolderSession, PID: int64(l.PID),
		XminAge: float64(*l.BackendXminAge), XactAgeS: curr.CollectedAt.Sub(*l.XactStart).Seconds()}
	if l.BackendStart != nil {
		h.BackendStart = *l.BackendStart
	}
	if l.State != nil {
		h.State = *l.State
	}
	return xminCandidate{holder: h, lock: l}, true
}

// lockRowOf finds a session holder's lock-graph row (a session row, or
// its waiting lock): same pid, and the same backend start when both are
// known (pids are reused).
func lockRowOf(curr *collector.Snapshot, h probes.XminHolder) *collector.LockInfo {
	if h.Kind != probes.HolderSession || h.PID <= 0 {
		return nil
	}
	for i := range curr.Locks {
		l := &curr.Locks[i]
		if int64(l.PID) != h.PID {
			continue
		}
		if l.BackendStart != nil && !h.BackendStart.IsZero() &&
			!l.BackendStart.Truncate(time.Microsecond).Equal(
				h.BackendStart.Truncate(time.Microsecond)) {
			continue
		}
		return l
	}
	return nil
}

// qualifies reports a holder the tree may name for tables.
func (r vacuumRules) qualifies(c xminCandidate, tables []string) bool {
	h := c.holder
	if !(h.XminAge > 0) {
		return false // unknown age (NULL backend_xmin and backend_xid)
	}
	switch h.Kind {
	case probes.HolderSession:
		return h.PID > 0 && secondsAtLeast(h.XactAgeS, r.holderFloor)
	case probes.HolderPreparedXact:
		return h.Name != "" && secondsAtLeast(h.XactAgeS, r.preparedFloor)
	case probes.HolderStandby:
		return h.PID > 0
	case probes.HolderSlot:
		return h.Name != ""
	case probes.HolderSlotCatalog:
		// catalog_xmin holds back only the system catalogs.
		return h.Name != "" && anyCatalogTable(tables)
	}
	return false
}

// secondsAtLeast compares an age in seconds with a floor, at microsecond
// precision (an age read as a float is never a hair short of its floor).
func secondsAtLeast(ageS float64, floor time.Duration) bool {
	age := time.Duration(ageS * float64(time.Second)).Round(time.Microsecond)
	return age >= floor
}

func anyCatalogTable(tables []string) bool {
	for _, t := range tables {
		if strings.HasPrefix(t, "pg_catalog.") {
			return true
		}
	}
	return false
}

// gidHash is the xmin_horizon query's name of a prepared transaction:
// the md5 of its gid.
func gidHash(gid string) string {
	sum := md5.Sum([]byte(gid))
	return hex.EncodeToString(sum[:])
}

// preparedByHash finds the collector's prepared transaction behind a
// holder named by gid hash.
func preparedByHash(curr *collector.Snapshot, hash string) (collector.PreparedTransaction, bool) {
	for _, p := range curr.PreparedXacts {
		if gidHash(p.GID) == hash {
			return p, true
		}
	}
	return collector.PreparedTransaction{}, false
}

// agedPrepared is the oldest (by XID age) of this database's prepared
// transactions at least floor old at the snapshot.
func agedPrepared(curr *collector.Snapshot, floor time.Duration) (
	collector.PreparedTransaction, bool,
) {
	var oldest collector.PreparedTransaction
	found := false
	for _, p := range curr.PreparedXacts {
		if curr.CollectedAt.Sub(p.Prepared) < floor {
			continue
		}
		if !found || p.XIDAge > oldest.XIDAge {
			oldest, found = p, true
		}
	}
	return oldest, found
}
