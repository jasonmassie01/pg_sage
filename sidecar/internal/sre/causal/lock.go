package causal

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// BlockerEvidence is the root-blocker identity M0 records on a
// lock_contention incident (rca.BlockerIdentity), cited by its evidence
// id. It is used when the lock graph cannot be probed.
type BlockerEvidence struct {
	EvidenceID   string
	PID          int
	State        string
	TotalBlocked int
	ChainDepth   int
}

// Scoring thresholds (seconds).
const (
	longXactSeconds  = 60
	shortXactSeconds = 10
)

// lockRoot is the features of the chosen root blocker.
type lockRoot struct {
	evidenceID string
	pid        int
	kind       string
	state      string
	xactAgeS   float64
	waiters    int
	ddlWaiter  *probes.LockEdge // first DDL-strength waiter blocked by the root
	behindDDL  int
	rowWaits   int
	anyDDL     bool
	fromGraph  bool
}

func (r lockRoot) subject() string {
	if r.kind == "prepared_xact" {
		return "prepared transaction"
	}
	return fmt.Sprintf("pid %d", r.pid)
}

// DiagnoseLock scores the lock-blocking hypotheses for the root blocker
// with the most waiters. It reads lock_graph and prepared_xacts
// observations and falls back to the incident's recorded blocker
// identities when the lock graph is unavailable.
func DiagnoseLock(obs []Observation, blockers []BlockerEvidence) Diagnosis {
	missing := missingFor(obs, probes.LockGraph, probes.PreparedXacts)
	root, rootOK, reason := chooseLockRoot(obs, blockers)
	if !rootOK {
		root = lockRoot{xactAgeS: math.NaN()}
	}
	prep, prepOK := find(obs, probes.PreparedXacts)
	hs := []Hypothesis{scoreIdle(root), scorePrepared(root, prep, prepOK),
		scoreDDL(root), scoreHotRow(root)}
	d := rank(FamilyLockBlocking, hs)
	d.Missing = missing
	if !rootOK {
		d.Root, d.Conclusive, d.Reason = nil, false, reason
		d.Alternatives = append(d.Alternatives, d.Contributing...)
		d.Contributing = nil
		return d
	}
	d.Subject = root.subject()
	return d
}

func chooseLockRoot(obs []Observation, blockers []BlockerEvidence) (lockRoot, bool, string) {
	g, ok := find(obs, probes.LockGraph)
	if ok && g.Result.Status.Usable() {
		edges, err := probes.LockEdges(g.Result)
		switch {
		case err != nil:
			return lockRoot{}, false, "lock graph unreadable: " + err.Error()
		case len(edges) == 0:
			return lockRoot{}, false, "no lock waits at probe time"
		}
		root, found := rootFromGraph(edges, g.EvidenceID)
		if !found {
			return lockRoot{}, false, "no root blocker: the waits form a cycle"
		}
		return root, true, ""
	}
	if root, found := rootFromBlockers(blockers); found {
		return root, true, ""
	}
	return lockRoot{}, false, "no lock evidence"
}

func rootFromBlockers(blockers []BlockerEvidence) (lockRoot, bool) {
	best := -1
	for i, b := range blockers {
		if best < 0 || b.TotalBlocked > blockers[best].TotalBlocked ||
			(b.TotalBlocked == blockers[best].TotalBlocked && b.PID < blockers[best].PID) {
			best = i
		}
	}
	if best < 0 {
		return lockRoot{}, false
	}
	b := blockers[best]
	return lockRoot{evidenceID: b.EvidenceID, pid: b.PID, kind: "backend",
		state: b.State, xactAgeS: math.NaN(), waiters: b.TotalBlocked}, true
}

// rootFromGraph picks the root blocker (a blocker that is not itself
// waiting) with the most transitive waiters; ties go to the lower pid.
func rootFromGraph(edges []probes.LockEdge, evidenceID string) (lockRoot, bool) {
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].BlockerPID != edges[j].BlockerPID {
			return edges[i].BlockerPID < edges[j].BlockerPID
		}
		return edges[i].WaiterPID < edges[j].WaiterPID
	})
	children := map[int][]probes.LockEdge{}
	waiting := map[int]bool{}
	for _, e := range edges {
		children[e.BlockerPID] = append(children[e.BlockerPID], e)
		waiting[e.WaiterPID] = true
	}
	var best lockRoot
	found := false
	for _, e := range edges {
		if waiting[e.BlockerPID] || (found && e.BlockerPID == best.pid) {
			continue
		}
		r := describeRoot(e, children, evidenceID)
		if !found || r.waiters > best.waiters {
			best, found = r, true
		}
	}
	return best, found
}

func describeRoot(head probes.LockEdge, children map[int][]probes.LockEdge,
	evidenceID string) lockRoot {
	r := lockRoot{evidenceID: evidenceID, pid: head.BlockerPID, kind: head.BlockerKind,
		state: head.BlockerState, xactAgeS: head.BlockerXactAgeS, fromGraph: true}
	sub := subtree(head.BlockerPID, children)
	r.waiters = len(sub.waiters)
	r.rowWaits, r.anyDDL = sub.rowWaits, sub.anyDDL
	for _, e := range children[head.BlockerPID] {
		if e.StrongRelationWait() && r.ddlWaiter == nil {
			edge := e
			r.ddlWaiter = &edge
			r.behindDDL = len(subtree(e.WaiterPID, children).waiters)
		}
	}
	return r
}

type subtreeStats struct {
	waiters  map[int]bool
	rowWaits int
	anyDDL   bool
}

// subtree walks the sessions waiting (transitively) on pid.
func subtree(pid int, children map[int][]probes.LockEdge) subtreeStats {
	st := subtreeStats{waiters: map[int]bool{}}
	queue := []int{pid}
	seen := map[int]bool{pid: true}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range children[cur] {
			if e.RowWait() {
				st.rowWaits++
			}
			st.anyDDL = st.anyDDL || e.StrongRelationWait()
			st.waiters[e.WaiterPID] = true
			if !seen[e.WaiterPID] {
				seen[e.WaiterPID] = true
				queue = append(queue, e.WaiterPID)
			}
		}
	}
	return st
}

func ageText(s float64) string { return probes.FormatValue(s) + " s" }

func scoreIdle(r lockRoot) Hypothesis {
	h := newHypothesis(IdleInTxHolder, r.subject())
	switch {
	case r.kind == "prepared_xact":
		h.contradict(r.evidenceID, "the root blocker is a prepared transaction, not a session")
	case strings.HasPrefix(r.state, "idle in transaction"):
		h.add(0.55, r.evidenceID, fmt.Sprintf("root blocker pid %d is %s", r.pid, r.state))
		if r.waiters > 0 {
			h.add(0.15, r.evidenceID, fmt.Sprintf("it blocks %d sessions", r.waiters))
		}
		if r.xactAgeS >= longXactSeconds {
			h.add(0.15, r.evidenceID, "its transaction has been open "+ageText(r.xactAgeS))
		}
	case r.state == "active":
		h.contradict(r.evidenceID, fmt.Sprintf("root blocker pid %d is active, not idle", r.pid))
	}
	return h
}

func scorePrepared(r lockRoot, prep Observation, prepOK bool) Hypothesis {
	h := newHypothesis(PreparedXactHolder, r.subject())
	if r.kind == "prepared_xact" {
		h.add(0.6, r.evidenceID, "the root blocker is a prepared transaction (pid 0)")
	}
	if !prepOK || !prep.Result.Status.Usable() {
		return h
	}
	xacts, err := probes.Prepared(prep.Result)
	if err != nil {
		return h
	}
	if len(xacts) == 0 {
		h.contradict(prep.EvidenceID, "no prepared transactions in this database")
		return h
	}
	h.add(0.15, prep.EvidenceID, fmt.Sprintf("%d prepared transactions exist", len(xacts)))
	oldest := math.NaN()
	for _, x := range xacts {
		if math.IsNaN(oldest) || x.AgeS > oldest {
			oldest = x.AgeS
		}
	}
	if oldest >= longXactSeconds {
		h.add(0.1, prep.EvidenceID, "the oldest was prepared "+ageText(oldest)+" ago")
	}
	return h
}

func scoreDDL(r lockRoot) Hypothesis {
	h := newHypothesis(DDLLockQueue, r.subject())
	if !r.fromGraph {
		return h
	}
	if !r.anyDDL {
		h.contradict(r.evidenceID,
			"no ACCESS EXCLUSIVE (or other DDL-strength) waiter in the lock queue")
		return h
	}
	if w := r.ddlWaiter; w != nil {
		h.add(0.45, r.evidenceID, fmt.Sprintf("pid %d waits for %s on %s behind the root",
			w.WaiterPID, w.RequestedMode, probes.FormatValue(w.Relation)))
	}
	if r.behindDDL > 0 {
		h.add(0.25, r.evidenceID, fmt.Sprintf("%d sessions queue behind that DDL",
			r.behindDDL))
	}
	if r.xactAgeS >= longXactSeconds {
		h.add(0.15, r.evidenceID, "the root's transaction has been open "+ageText(r.xactAgeS))
	}
	return h
}

func scoreHotRow(r lockRoot) Hypothesis {
	h := newHypothesis(HotRowContention, r.subject())
	if !r.fromGraph {
		return h
	}
	if r.rowWaits == 0 {
		h.contradict(r.evidenceID, "the waits are on relation locks, not rows")
		return h
	}
	if r.rowWaits >= 2 {
		h.add(0.45, r.evidenceID, fmt.Sprintf("%d row-lock waits (transactionid/tuple)",
			r.rowWaits))
	} else {
		h.add(0.2, r.evidenceID, "one row-lock wait (transactionid/tuple)")
	}
	if r.state == "active" {
		h.add(0.15, r.evidenceID, fmt.Sprintf("root blocker pid %d is active", r.pid))
	}
	if r.xactAgeS < shortXactSeconds {
		h.add(0.1, r.evidenceID, "its transaction is short ("+ageText(r.xactAgeS)+")")
	}
	return h
}
