package action

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Ineligible is why an investigation proposes no action.
type Ineligible struct {
	Reason ActionReason
	Detail string
}

func ineligible(r ActionReason, format string, args ...any) *Ineligible {
	return &Ineligible{Reason: r, Detail: fmt.Sprintf(format, args...)}
}

// cancelCandidate is a cancel derived from cited evidence only: the root
// blocker's identity anchor and the waiters behind it.
type cancelCandidate struct {
	Family       string
	Node         string
	PID          int32
	BackendStart time.Time
	QueryID      int64
	XactAgeS     float64
	EvidenceIDs  []sre.UUID
	Baseline     Baseline
}

// hotRowHolderSeconds: a row-lock root is one backend's doing only when
// its transaction is this old; shorter ones are contention.
const hotRowHolderSeconds = 60

// deriveCancel decides whether a concluded investigation proposes an
// evidence-matched cancel. The target comes only from the lock graph
// evidence its root hypothesis cites (AI-SRE-SPEC §2.9): never from text.
func deriveCancel(inv sre.Investigation, hyps []sre.HypothesisRecord,
	ev []sre.Evidence) (cancelCandidate, *Ineligible) {
	root, why := rootHypothesis(inv, hyps)
	if why != nil {
		return cancelCandidate{}, why
	}
	if why := eligibleRoot(inv, root); why != nil {
		return cancelCandidate{}, why
	}
	graph, ok := citedLockGraph(root, ev)
	if !ok {
		return cancelCandidate{}, ineligible(ReasonLockEvidenceMissing,
			"the root hypothesis cites no usable lock graph evidence")
	}
	edges, err := decodeLockGraph(graph)
	if err != nil {
		return cancelCandidate{}, ineligible(ReasonLockEvidenceMissing,
			"the lock graph evidence is unreadable: %v", err)
	}
	c, why := candidateFromGraph(edges, graph.ID)
	if why != nil {
		return c, why
	}
	return finishCandidate(inv, root, c)
}

func rootHypothesis(inv sre.Investigation, hyps []sre.HypothesisRecord) (sre.HypothesisRecord,
	*Ineligible) {
	if inv.State != sre.StateConcluded {
		return sre.HypothesisRecord{}, ineligible(ReasonNotConcluded,
			"the investigation is %s, not concluded", inv.State)
	}
	if inv.TriggerKind != sre.TriggerLock && inv.TriggerKind != sre.TriggerConnections {
		return sre.HypothesisRecord{}, ineligible(ReasonUnsupportedFamily,
			"no supported action for %s investigations", inv.TriggerKind)
	}
	for _, h := range hyps {
		if h.Status == sre.HypothesisRoot {
			return h, nil
		}
	}
	return sre.HypothesisRecord{}, ineligible(ReasonNotConcluded,
		"the investigation has no root cause")
}

// eligibleRoot rules out mechanisms a single cancel does not fix.
func eligibleRoot(inv sre.Investigation, root sre.HypothesisRecord) *Ineligible {
	switch root.Node {
	case "ddl_lock_queue", "hot_row_contention", "blocked_backlog":
		return nil
	case "idle_in_tx_holder":
		return idleInTransaction(root.Subject)
	case "prepared_xact_holder":
		return ineligible(ReasonRootNotBackend, "the root blocker is a prepared "+
			"transaction: no backend to cancel; resolve it with COMMIT or ROLLBACK PREPARED")
	}
	return ineligible(ReasonUnsupportedRoot, "a cancel does not address %s",
		strings.ReplaceAll(root.Node, "_", " "))
}

func idleInTransaction(subject string) *Ineligible {
	return ineligible(ReasonIdleInTransaction, "the root blocker (%s) is idle in "+
		"transaction: pg_cancel_backend does not end an idle transaction (it has no "+
		"running query); end it from its application or terminate the session through "+
		"your normal process", subject)
}

// citedLockGraph is the usable lock_graph evidence the root cites.
func citedLockGraph(root sre.HypothesisRecord, ev []sre.Evidence) (sre.Evidence, bool) {
	cited := map[sre.UUID]bool{}
	for _, f := range root.Support {
		cited[f.EvidenceID] = true
	}
	for _, e := range ev {
		if cited[e.ID] && e.ProbeID == string(probes.LockGraph) {
			return e, true
		}
	}
	return sre.Evidence{}, false
}

func decodeLockGraph(e sre.Evidence) ([]probes.EdgeIdentity, error) {
	var res probes.Result
	dec := json.NewDecoder(bytes.NewReader(e.Payload))
	dec.UseNumber()
	if err := dec.Decode(&res); err != nil {
		return nil, err
	}
	return probes.LockEdgeIdentities(res)
}

// candidateFromGraph picks the root blocker: a blocker that is not itself
// waiting, with the most transitive waiters; ties go to the lower pid.
func candidateFromGraph(edges []probes.EdgeIdentity, evidenceID sre.UUID) (cancelCandidate,
	*Ineligible) {
	if len(edges) == 0 {
		return cancelCandidate{}, ineligible(ReasonNoWaiters,
			"the lock graph showed no waits")
	}
	children := map[int][]probes.EdgeIdentity{}
	waiting := map[int]bool{}
	for _, e := range edges {
		children[e.BlockerPID] = append(children[e.BlockerPID], e)
		waiting[e.WaiterPID] = true
	}
	best, bestWaiters := -1, -1
	for pid := range children {
		if waiting[pid] {
			continue
		}
		n := len(transitiveWaiters(pid, children))
		if n > bestWaiters || (n == bestWaiters && pid < best) {
			best, bestWaiters = pid, n
		}
	}
	if best < 0 {
		return cancelCandidate{}, ineligible(ReasonNoWaiters,
			"no root blocker: the waits form a cycle")
	}
	return rootCandidate(best, children, evidenceID)
}

func rootCandidate(pid int, children map[int][]probes.EdgeIdentity,
	evidenceID sre.UUID) (cancelCandidate, *Ineligible) {
	head := children[pid][0]
	switch {
	case head.BlockerKind == "prepared_xact" || pid <= 0:
		return cancelCandidate{}, ineligible(ReasonRootNotBackend,
			"the root blocker is a prepared transaction: no backend to cancel")
	case strings.HasPrefix(head.BlockerState, "idle in transaction"):
		return cancelCandidate{}, idleInTransaction(fmt.Sprintf("pid %d", pid))
	case head.BlockerState != "active":
		return cancelCandidate{}, ineligible(ReasonTargetNotActive,
			"the root blocker pid %d is %q, not running a statement", pid,
			head.BlockerState)
	case head.BlockerBackendStart.IsZero():
		return cancelCandidate{}, ineligible(ReasonLockEvidenceMissing,
			"the lock graph lacks the root blocker's backend_start")
	}
	waiters := transitiveWaiters(pid, children)
	return cancelCandidate{PID: int32(pid), BackendStart: head.BlockerBackendStart,
		QueryID: head.BlockerQueryID, XactAgeS: head.BlockerXactAgeS,
		EvidenceIDs: []sre.UUID{evidenceID},
		Baseline:    Baseline{Waiting: len(waiters), Waiters: waiters}}, nil
}

// transitiveWaiters lists the sessions waiting (transitively) on pid, in
// pid order.
func transitiveWaiters(pid int, children map[int][]probes.EdgeIdentity) []Waiter {
	seen := map[int]bool{pid: true}
	var out []Waiter
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, e := range children[cur] {
			if seen[e.WaiterPID] {
				continue
			}
			seen[e.WaiterPID] = true
			out = append(out, Waiter{PID: int32(e.WaiterPID),
				BackendStart: e.WaiterBackendStart})
			queue = append(queue, e.WaiterPID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// finishCandidate checks the candidate against the hypothesis it came
// from and adds the hypothesis' other cited evidence.
func finishCandidate(inv sre.Investigation, root sre.HypothesisRecord,
	c cancelCandidate) (cancelCandidate, *Ineligible) {
	if inv.TriggerKind == sre.TriggerLock && root.Subject != fmt.Sprintf("pid %d", c.PID) {
		return cancelCandidate{}, ineligible(ReasonEvidenceInconsistent,
			"the root hypothesis names %q but the lock graph's root is pid %d",
			root.Subject, c.PID)
	}
	if root.Node == "hot_row_contention" && !(c.XactAgeS >= hotRowHolderSeconds) {
		return cancelCandidate{}, ineligible(ReasonShortContention, "the row-lock "+
			"waits are contention among short transactions (the holder's is %s s): "+
			"cancelling one backend does not remove the mechanism",
			probes.FormatValue(c.XactAgeS))
	}
	c.Family, c.Node = string(root.Family), root.Node
	if c.Family == "" {
		c.Family = inv.Summary.Family
	}
	for _, f := range root.Support {
		if !containsUUID(c.EvidenceIDs, f.EvidenceID) {
			c.EvidenceIDs = append(c.EvidenceIDs, f.EvidenceID)
		}
	}
	return c, nil
}

func containsUUID(ids []sre.UUID, id sre.UUID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
