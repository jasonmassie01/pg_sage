package rca

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Perf F9.2 (dogfood lifeos): every persist pass (analyzer cycle, lock
// fast path, detector episodes) rewrote every tracked incident, including
// its 2.8-45 kB TOASTed causal chain: 3,406 updates for 4 open incidents.
// A persisted incident whose stored fields did not change since its last
// write is not written again; one read per pass still notices operators'
// resolutions and deletions (R04).

// persistedFields are the incident fields updateIncidentSQL stores.
type persistedFields struct {
	LastDetectedAt   time.Time   `json:"l"`
	Severity         string      `json:"s"`
	RootCause        string      `json:"r"`
	CausalChain      []ChainLink `json:"c"`
	OccurrenceCount  int         `json:"o"`
	EscalatedAt      *time.Time  `json:"e"`
	IdentityKey      string      `json:"i"`
	DatabaseName     string      `json:"d"`
	ResolvedAt       *time.Time  `json:"ra"`
	ResolvedBy       string      `json:"rb"`
	ResolutionReason string      `json:"rr"`
}

// storedFingerprint identifies the stored content of an incident.
func storedFingerprint(inc *Incident) string {
	raw, err := json.Marshal(persistedFields{
		LastDetectedAt: inc.LastDetectedAt.UTC(), Severity: inc.Severity,
		RootCause: inc.RootCause, CausalChain: inc.CausalChain,
		OccurrenceCount: inc.OccurrenceCount, EscalatedAt: inc.EscalatedAt,
		IdentityKey: identityKey(inc), DatabaseName: inc.DatabaseName,
		ResolvedAt: inc.ResolvedAt, ResolvedBy: inc.ResolvedBy,
		ResolutionReason: inc.ResolutionReason,
	})
	if err != nil {
		return "" // never equal to a stored fingerprint: the row is written
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// unchanged reports whether it needs no write this pass.
func (it persistItem) unchanged() bool {
	return it.persisted && it.written != "" && it.written == storedFingerprint(&it.inc)
}

const readStoredStateSQL = `/* pg_sage */
SELECT id::text, resolved_at, COALESCE(resolved_by, ''), COALESCE(resolution_reason, '')
FROM sage.incidents WHERE id = ANY($1::uuid[])`

// checkUnchanged reads, in one statement, what happened to the rows of
// incidents this pass did not write: resolved outside the engine (the
// resolution is adopted) or deleted (dropped from memory). An open row
// needs no result.
func checkUnchanged(
	ctx context.Context, pool *pgxpool.Pool, ids []string,
) ([]persistResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, readStoredStateSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("rca: read stored incidents: %w", err)
	}
	stored, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (persistResult, error) {
		var res persistResult
		ext := Incident{}
		err := r.Scan(&res.id, &ext.ResolvedAt, &ext.ResolvedBy, &ext.ResolutionReason)
		if ext.ResolvedAt != nil {
			res.resolved, res.external = true, &ext
		}
		return res, err
	})
	if err != nil {
		return nil, fmt.Errorf("rca: read stored incidents: %w", err)
	}
	return unchangedResults(ids, stored), nil
}

func unchangedResults(ids []string, stored []persistResult) []persistResult {
	found := make(map[string]persistResult, len(stored))
	for _, r := range stored {
		found[r.id] = r
	}
	var out []persistResult
	for _, id := range ids {
		r, ok := found[id]
		switch {
		case !ok:
			out = append(out, persistResult{id: id, gone: true})
		case r.resolved:
			out = append(out, r)
		}
	}
	return out
}
