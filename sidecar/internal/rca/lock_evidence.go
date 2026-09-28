package rca

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// maxEvidenceBlockers bounds how many root blockers one lock_contention
// incident records (the ones blocking the most sessions).
const maxEvidenceBlockers = 5

// BlockerIdentity pins one root blocker the way the executor re-checks a
// backend before an approved cancel/terminate (P0-06): pid plus
// backend_start (PIDs are reused) plus the query identity. The query
// itself is recorded only as a hash of its first 200 characters.
type BlockerIdentity struct {
	PID            int       `json:"pid"`
	BackendStart   time.Time `json:"backend_start"`
	QueryStart     time.Time `json:"query_start"`
	QueryID        int64     `json:"query_id"`
	QuerySHA       string    `json:"query_sha,omitempty"`
	State          string    `json:"state"`
	TotalBlocked   int       `json:"total_blocked"`
	ChainDepth     int       `json:"chain_depth"`
	LockedRelation string    `json:"locked_relation,omitempty"`
	LockMode       string    `json:"lock_mode,omitempty"`
}

// blockersFromFindings extracts root-blocker identities from lock_chain
// findings, most-blocking first, capped at maxEvidenceBlockers. Findings
// without a usable pid contribute no identity.
func blockersFromFindings(findings []analyzer.Finding) []BlockerIdentity {
	var out []BlockerIdentity
	for _, f := range findings {
		if f.Category != "lock_chain" {
			continue
		}
		if b, ok := blockerFromDetail(f.Detail); ok {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TotalBlocked != out[j].TotalBlocked {
			return out[i].TotalBlocked > out[j].TotalBlocked
		}
		return out[i].PID < out[j].PID
	})
	if len(out) > maxEvidenceBlockers {
		out = out[:maxEvidenceBlockers]
	}
	return out
}

func blockerFromDetail(d map[string]any) (BlockerIdentity, bool) {
	pid, ok := d["pid"].(int)
	if !ok || pid <= 0 {
		return BlockerIdentity{}, false
	}
	b := BlockerIdentity{PID: pid}
	b.BackendStart, _ = d["backend_start"].(time.Time)
	b.QueryStart, _ = d["query_start"].(time.Time)
	b.QueryID, _ = d["query_id"].(int64)
	b.State, _ = d["root_blocker_state"].(string)
	b.TotalBlocked, _ = d["total_blocked"].(int)
	b.ChainDepth, _ = d["chain_depth"].(int)
	b.LockedRelation, _ = d["locked_relation"].(string)
	b.LockMode, _ = d["blocker_mode"].(string)
	if q, ok := d["query"].(string); ok && q != "" {
		sum := sha256.Sum256([]byte(q))
		b.QuerySHA = hex.EncodeToString(sum[:8])
	}
	return b, true
}

// lockContentionIncident builds the lock_contention incident: a summary
// link plus one link per root blocker carrying its identity. It refreshes
// the open incident's evidence when merged (liveEvidence).
func lockContentionIncident(sig *Signal) Incident {
	chain := []ChainLink{{
		Order: 1, Signal: sig.ID,
		Description: "Lock chain contention detected",
		Evidence: fmt.Sprintf("%d lock chains, %d total blocked",
			intMetric(sig, "lock_chain_count"),
			intMetric(sig, "total_blocked")),
	}}
	for i := range sig.blockers {
		b := sig.blockers[i]
		chain = append(chain, ChainLink{
			Order: i + 2, Signal: sig.ID,
			Description: blockerDescription(b),
			Evidence:    blockerEvidence(b),
			Blocker:     &b,
		})
	}
	inc := buildIncident(sig.FiredAt, sig.Severity, []string{sig.ID},
		lockRootCause(sig.blockers), chain, nil, "", "safe")
	inc.liveEvidence = true
	return inc
}

func lockRootCause(blockers []BlockerIdentity) string {
	if len(blockers) == 0 {
		return "Lock chain contention detected"
	}
	b := blockers[0]
	return fmt.Sprintf("Lock chain: root blocker pid %d (%s) blocks %d "+
		"sessions", b.PID, stateOrUnknown(b.State), b.TotalBlocked)
}

func blockerDescription(b BlockerIdentity) string {
	desc := fmt.Sprintf("Root blocker pid %d (%s) blocks %d sessions, "+
		"chain depth %d", b.PID, stateOrUnknown(b.State), b.TotalBlocked,
		b.ChainDepth)
	if b.LockedRelation != "" {
		desc += " on " + b.LockedRelation
	}
	return desc
}

func blockerEvidence(b BlockerIdentity) string {
	return fmt.Sprintf("pid=%d backend_start=%s query_start=%s "+
		"query_id=%d query_sha=%s blocked=%d depth=%d mode=%s",
		b.PID, b.BackendStart.UTC().Format(time.RFC3339Nano),
		b.QueryStart.UTC().Format(time.RFC3339Nano), b.QueryID,
		b.QuerySHA, b.TotalBlocked, b.ChainDepth, b.LockMode)
}

func stateOrUnknown(state string) string {
	if state == "" {
		return "unknown state"
	}
	return state
}
