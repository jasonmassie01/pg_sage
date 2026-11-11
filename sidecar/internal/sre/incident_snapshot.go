package sre

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/sre/causal"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Detection snapshot (sre-05). A lock investigation probes at least one
// trigger poll after detection; by then many chains have cleared. On its
// first step it therefore reads the incident's recorded root blockers
// (rca.BlockerIdentity in sage.incidents.causal_chain) once and commits
// them as incident_snapshot evidence, observed when RCA last recorded
// the chain. Because the snapshot is committed evidence, resumes and
// replays diagnose the same input. Only structured identity fields are
// kept: no application name, no free text, the query as its hash.

// IncidentSnapshotProbeID is the evidence id of a detection snapshot. It
// is no catalog probe: it reads pg_sage's own sage.incidents.
const IncidentSnapshotProbeID probes.ID = "incident_snapshot"

// Snapshot reasons.
const (
	ReasonMalformedChain     = "malformed_causal_chain"
	ReasonIncidentUnreadable = "incident_unreadable"
	snapshotTooOld           = "detection snapshot too old"
	snapshotUndated          = "detection time unknown"
)

// Snapshot bounds: the blockers kept (as RCA records them), the read's
// total time, and its statement and lock timeouts on the database.
const (
	maxSnapshotBlockers     = 5
	incidentSnapshotTimeout = 2 * time.Second
	snapshotStatementMS     = "2000"
	snapshotLockMS          = "500"
)

// IncidentSnapshot is an incident's recorded causal chain and when RCA
// last recorded it.
type IncidentSnapshot struct {
	CausalChain []byte
	DetectedAt  time.Time
}

// IncidentSnapshotSource reads an incident's detection snapshot; found is
// false for an incident it does not hold.
type IncidentSnapshotSource interface {
	IncidentSnapshot(ctx context.Context, incidentID string) (IncidentSnapshot, bool, error)
}

var _ IncidentSnapshotSource = (*PGTriggerSource)(nil)

// IncidentSnapshot reads one incident of this source's database scope in
// a read-only transaction bounded by statement and lock timeouts. An id
// that is not a UUID is not found.
func (s *PGTriggerSource) IncidentSnapshot(ctx context.Context,
	incidentID string) (IncidentSnapshot, bool, error) {
	if s == nil || s.pool == nil {
		return IncidentSnapshot{}, false, fmt.Errorf("%w: incident source has no database",
			ErrInvalidRequest)
	}
	id, err := ParseUUID(incidentID)
	if err != nil {
		return IncidentSnapshot{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, incidentSnapshotTimeout)
	defer cancel()
	var snap IncidentSnapshot
	var chain *string
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true),
				set_config('lock_timeout', $2, true)`, snapshotStatementMS,
				snapshotLockMS); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT causal_chain::text,
				    COALESCE(last_detected_at, detected_at)
				FROM sage.incidents
				WHERE id = $1::uuid AND (database_name IS NULL OR database_name = $2)`,
				string(id), s.database).Scan(&chain, &snap.DetectedAt)
		})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return IncidentSnapshot{}, false, nil
	case err != nil:
		return IncidentSnapshot{}, false, fmt.Errorf("read incident %s snapshot: %w", id, err)
	}
	if chain != nil {
		snap.CausalChain = []byte(*chain)
	}
	return snap, true, nil
}

// incidentSnapshot is the snapshot evidence of a lock investigation's
// incident: none for another family, without an incident or a source,
// or when the incident records no blocker. A failed read is error
// evidence (incident_unreadable), never an investigation failure.
func (c *Coordinator) incidentSnapshot(ctx context.Context, inv Investigation) []probes.Result {
	if c.incidents == nil || inv.TriggerKind != TriggerLock || inv.IncidentID == "" {
		return nil
	}
	snap, found, err := c.incidents.IncidentSnapshot(ctx, inv.IncidentID)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil // the lease was lost: the step commit is refused anyway
	case err != nil:
		c.logFn("WARN", "sre: investigation %s: reading the detection snapshot of "+
			"incident %s failed; continuing without it: %v", inv.ID, inv.IncidentID, err)
		return []probes.Result{{ProbeID: IncidentSnapshotProbeID, Version: "v1",
			Status: probes.StatusError, Reason: ReasonIncidentUnreadable,
			ObservedAt: c.now()}}
	case !found:
		return nil
	}
	if res, ok := snapshotResult(snap); ok {
		return []probes.Result{res}
	}
	return nil
}

// snapshotBlocker is the part of an rca.BlockerIdentity a snapshot keeps,
// plus a raw query to hash should a chain carry one.
type snapshotBlocker struct {
	PID          int       `json:"pid"`
	BackendStart time.Time `json:"backend_start"`
	State        string    `json:"state"`
	TotalBlocked int       `json:"total_blocked"`
	ChainDepth   int       `json:"chain_depth"`
	QuerySHA     string    `json:"query_sha"`
	Query        string    `json:"query"`
}

// snapshotResult is the evidence of a detection snapshot; false when the
// chain records no blocker. A chain that is not a list of root-blocker
// identities is error evidence (malformed_causal_chain), never a blocker.
func snapshotResult(snap IncidentSnapshot) (probes.Result, bool) {
	res := probes.Result{ProbeID: IncidentSnapshotProbeID, Version: "v1",
		ObservedAt: snap.DetectedAt}
	blockers, err := chainBlockers(snap.CausalChain)
	switch {
	case err != nil:
		res.Status, res.Reason, res.Error = probes.StatusError, ReasonMalformedChain, err.Error()
		return res, true
	case len(blockers) == 0:
		return probes.Result{}, false
	}
	res.Status = probes.StatusOK
	for _, b := range blockers {
		res.Rows = append(res.Rows, b.row())
	}
	return res, true
}

// chainBlockers decodes the chain's blockers, most blocking first (ties:
// lower pid), at most maxSnapshotBlockers. Its errors never echo the
// incident's text.
func chainBlockers(raw []byte) ([]snapshotBlocker, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var links []struct {
		Blocker json.RawMessage `json:"blocker"`
	}
	if json.Unmarshal(raw, &links) != nil {
		return nil, errors.New("causal chain is not a list of links")
	}
	var out []snapshotBlocker
	for i, l := range links {
		if len(l.Blocker) == 0 || bytes.Equal(l.Blocker, []byte("null")) {
			continue
		}
		var b snapshotBlocker
		if json.Unmarshal(l.Blocker, &b) != nil || b.PID <= 0 || b.TotalBlocked < 0 ||
			b.ChainDepth < 0 {
			return nil, fmt.Errorf("causal chain link %d: blocker is not a root-blocker "+
				"identity", i+1)
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TotalBlocked != out[j].TotalBlocked {
			return out[i].TotalBlocked > out[j].TotalBlocked
		}
		return out[i].PID < out[j].PID
	})
	return out[:min(len(out), maxSnapshotBlockers)], nil
}

// backendStates are pg_stat_activity's states; any other text is unknown.
var backendStates = map[string]bool{"active": true, "idle": true,
	"idle in transaction": true, "idle in transaction (aborted)": true,
	"fastpath function call": true, "disabled": true}

var querySHAPattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

func (b snapshotBlocker) row() probes.Row {
	state := "unknown"
	if backendStates[b.State] {
		state = b.State
	}
	r := probes.Row{"pid": int64(b.PID), "state": state,
		"total_blocked": int64(b.TotalBlocked), "chain_depth": int64(b.ChainDepth)}
	if !b.BackendStart.IsZero() {
		r["backend_start"] = b.BackendStart.UTC()
	}
	switch {
	case querySHAPattern.MatchString(b.QuerySHA):
		r["query_sha"] = b.QuerySHA
	case b.Query != "":
		// As RCA hashes it: the first 200 characters, 8 bytes of SHA-256.
		sum := sha256.Sum256([]byte(truncateRunes(b.Query, 200)))
		r["query_sha"] = hex.EncodeToString(sum[:8])
	}
	return r
}

// diagnoseLock is the lock family's diagnosis with the committed
// detection snapshot as its blocker input. An unusable snapshot (stale,
// undated, unreadable or malformed) is named as missing and not used.
func diagnoseLock(obs []causal.Observation) causal.Diagnosis {
	blockers, missing := snapshotBlockers(obs)
	d := causal.DiagnoseLock(obs, blockers)
	if missing != nil {
		d.Missing = append(d.Missing, *missing)
	}
	return d
}

// snapshotBlockers decodes the newest snapshot observation, dated at
// detection and cited by its evidence id.
func snapshotBlockers(obs []causal.Observation) ([]causal.BlockerEvidence, *causal.Missing) {
	var snap *causal.Observation
	for i := range obs {
		if obs[i].Result.ProbeID == IncidentSnapshotProbeID {
			snap = &obs[i]
		}
	}
	if snap == nil {
		return nil, nil
	}
	res := snap.Result
	missing := &causal.Missing{ProbeID: IncidentSnapshotProbeID, Status: res.Status,
		Reason: res.Reason}
	switch {
	case res.Reason == ReasonStaleEvidence:
		missing.Reason = snapshotTooOld
		return nil, missing
	case !res.Status.Usable():
		return nil, missing
	case res.ObservedAt.IsZero():
		missing.Status, missing.Reason = probes.StatusError, snapshotUndated
		return nil, missing
	}
	out := make([]causal.BlockerEvidence, 0, len(res.Rows))
	for _, row := range res.Rows {
		b, ok := blockerOfRow(row)
		if !ok {
			missing.Status, missing.Reason = probes.StatusError, ReasonMalformedChain
			return nil, missing
		}
		b.EvidenceID, b.DetectedAt = snap.EvidenceID, res.ObservedAt
		out = append(out, b)
	}
	return out, nil
}

// liveClaims counts the verified claims that cite evidence other than
// the detection snapshot. A model root, contest or unmodeled cause whose
// only support is what RCA recorded at detection is uncited: like the
// graph's, a detection-time root is never conclusive (sre-05).
func liveClaims(n *Narrative, stored []Evidence) int {
	if n == nil {
		return 0
	}
	snapshot := map[UUID]bool{}
	for _, e := range stored {
		if probes.ID(e.ProbeID) == IncidentSnapshotProbeID {
			snapshot[e.ID] = true
		}
	}
	kept := 0
	for _, c := range n.Claims {
		for _, id := range c.EvidenceIDs {
			if !snapshot[id] {
				kept++
				break
			}
		}
	}
	return kept
}

// blockerOfRow decodes one snapshot row through its JSON form, so stored
// (json.Number, RFC 3339 text) and in-memory rows read the same.
func blockerOfRow(row probes.Row) (causal.BlockerEvidence, bool) {
	raw, err := json.Marshal(row)
	if err != nil {
		return causal.BlockerEvidence{}, false
	}
	var b snapshotBlocker
	if json.Unmarshal(raw, &b) != nil || b.PID <= 0 {
		return causal.BlockerEvidence{}, false
	}
	return causal.BlockerEvidence{PID: b.PID, State: b.State, TotalBlocked: b.TotalBlocked,
		ChainDepth: b.ChainDepth, BackendStart: b.BackendStart}, true
}
