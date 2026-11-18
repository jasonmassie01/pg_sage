package runway

import (
	"context"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// ScopeCluster is the detail scope of a finding about the whole cluster
// (forecast-09); a finding without one is about its database.
const ScopeCluster = "cluster"

// clusterScope is one tick's ownership of the cluster series. Outside a
// shared set (no Owners, no system identifier, a standby) the monitor
// reports everything itself, as a single database does.
type clusterScope struct {
	key     string
	owner   string
	members []string
	shared  bool
	isOwner bool
}

// reports says whether this monitor reports the cluster series.
func (s clusterScope) reports() bool { return !s.shared || s.isOwner }

// isClusterRunway reports a finding about the cluster's series: the xid
// and mxid age, disk use and every replication slot. Overdue tables and
// sequences belong to their database.
func isClusterRunway(category, ident string) bool {
	return category == CategoryWAL || category == CategoryWraparound &&
		(ident == probes.RunwayXID || ident == probes.RunwayMXID)
}

// ownerTTL is how long a claim stays live: a monitor that misses two
// ticks hands the cluster to the next database.
func (m *Monitor) ownerTTL() time.Duration { return 2 * m.opts.Interval }

// claimCluster heartbeats the monitor on its cluster's key and returns
// this tick's scope. A tick whose WAL reading failed keeps the last key;
// a new key (restart, failover) leaves the old one.
func (m *Monitor) claimCluster(snap Snapshot) clusterScope {
	if m.opts.Owners == nil {
		return clusterScope{}
	}
	m.ownMu.Lock()
	defer m.ownMu.Unlock()
	key := m.ownKey
	if snap.WAL != nil {
		key = OwnerKey(*snap.WAL)
		if snap.WAL.InRecovery {
			key = "" // a standby reports nothing; it holds no claim
		}
		m.noteOwnerKey(snap.WAL, key)
	}
	if key != m.ownKey {
		m.opts.Owners.Leave(m.ownKey, m.opts.Database)
		m.ownKey = key
	}
	if key == "" {
		return clusterScope{}
	}
	owner, ok := m.opts.Owners.Claim(key, m.opts.Database, m.ownerTTL())
	return clusterScope{key: key, owner: owner, members: m.opts.Owners.Members(key),
		shared: true, isOwner: ok}
}

// noteOwnerKey logs once when a primary's system identifier becomes
// unreadable: its cluster series are then reported per database.
func (m *Monitor) noteOwnerKey(w *probes.WALRunway, key string) {
	noKey := key == "" && !w.InRecovery
	if noKey && !m.noOwnerKey {
		m.logFn("WARN", "runway: db %q: the cluster's system identifier is unreadable; "+
			"its cluster runways are reported for this database alone", m.opts.Database)
	}
	m.noOwnerKey = noKey
}

// leaveCluster drops the monitor's claim (its runtime ended).
func (m *Monitor) leaveCluster() {
	m.ownMu.Lock()
	defer m.ownMu.Unlock()
	m.opts.Owners.Leave(m.ownKey, m.opts.Database)
	m.ownKey = ""
}

// apply narrows a tick to the monitor's scope. The owner's cluster
// findings say so in their detail. A non-owner reports no cluster runway,
// holds none stale and does not resolve the WAL category itself (report
// keeps its xid and mxid rows open too): its duplicates are resolved only
// once the owner has reported (resolveDuplicates).
func (s clusterScope) apply(runways []Runway, evaluated []string,
	held map[string]map[string]string) ([]Runway, []string, map[string]map[string]string) {
	if !s.shared {
		return runways, evaluated, held
	}
	out := make([]Runway, 0, len(runways))
	for _, r := range runways {
		if !isClusterRunway(r.Category, r.Identifier) {
			out = append(out, r)
			continue
		}
		if s.isOwner {
			r.Detail = s.annotate(r.Detail)
			out = append(out, r)
		}
	}
	if s.isOwner || held == nil {
		return out, s.ownedCategories(evaluated), held
	}
	kept := map[string]map[string]string{}
	for cat, idents := range held {
		for ident, reason := range idents {
			if isClusterRunway(cat, ident) {
				continue
			}
			if kept[cat] == nil {
				kept[cat] = map[string]string{}
			}
			kept[cat][ident] = reason
		}
	}
	return out, without(evaluated, CategoryWAL), kept
}

// ownedCategories is evaluated less the WAL category for a non-owner.
func (s clusterScope) ownedCategories(evaluated []string) []string {
	if s.reports() {
		return evaluated
	}
	return without(evaluated, CategoryWAL)
}

// annotate adds the owner's scope to a cluster finding's detail.
func (s clusterScope) annotate(detail map[string]any) map[string]any {
	out := make(map[string]any, len(detail)+3)
	for k, v := range detail {
		out[k] = v
	}
	out["scope"], out["owner_database"] = ScopeCluster, s.owner
	out["cluster_databases"] = append([]string(nil), s.members...)
	return out
}

// keepCluster keeps a non-owner's open xid and mxid rows from resolving
// with the wraparound category: they are the owner's to report.
func (s clusterScope) keepCluster(cat string, keep map[string]bool) {
	if s.reports() || cat != CategoryWraparound {
		return
	}
	keep[probes.RunwayXID], keep[probes.RunwayMXID] = true, true
}

// resolveDuplicatesSQL resolves a non-owner's open cluster findings,
// naming the owner that reports them now: every finding of the cluster
// categories ($2: the WAL and multixact member runways) and the xid and
// mxid rows of the wraparound category.
const resolveDuplicatesSQL = `/* pg_sage */ UPDATE sage.findings
	SET status = 'resolved', resolved_at = now(),
	    detail = CASE WHEN jsonb_typeof(detail) = 'object' THEN detail
	                  ELSE '{}'::jsonb END || jsonb_build_object('cluster_owner', $1::text)
	WHERE status = 'open' AND (category = ANY ($2::text[]) OR
	      (category = $3 AND object_identifier = ANY ($4::text[])))`

// resolveDuplicates resolves a non-owner's open cluster findings once its
// owner has reported a successful evaluation within the claim ttl. An
// owner that has not (failing, just elected) leaves them open.
func (m *Monitor) resolveDuplicates(ctx context.Context, s clusterScope) (int, error) {
	if s.reports() || !m.opts.Owners.OwnerEvaluated(s.key, s.owner, m.ownerTTL()) {
		return 0, nil
	}
	tag, err := m.pool.Exec(ctx, resolveDuplicatesSQL, s.owner,
		[]string{CategoryWAL, CategoryMXMembers}, CategoryWraparound,
		[]string{probes.RunwayXID, probes.RunwayMXID})
	if err != nil {
		return 0, fmt.Errorf("resolve the cluster findings %q reports: %w", s.owner, err)
	}
	n := int(tag.RowsAffected())
	if n > 0 {
		m.logFn("INFO", "runway: db %q: resolved %d cluster runway finding(s); database "+
			"%q reports the cluster now", m.opts.Database, n, s.owner)
	}
	return n, nil
}

// clusterEvaluated says whether a tick evaluated every cluster series
// completely: only then does the owner's report clear its duplicates.
func clusterEvaluated(in EvalInput) bool {
	return in.TrendsOK && in.trendsComplete(CategoryWAL) &&
		in.trendsComplete(CategoryWraparound)
}
