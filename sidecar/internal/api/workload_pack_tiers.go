package api

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The workload pack's redaction tiers (evid-02c). GET {sha}?tier=model is
// a viewer read: the owner sees exactly what would leave the network. GET
// {sha}/export is admin only, like the audit pack's download, because the
// export holds the local tier's statement texts; it is signed and recorded
// in sage.auth_audit as evidence_exported.

// WorkloadPackKeys are the keys the tier routes use: Signing signs exports
// (nil refuses them), Pseudonym keys HashIdentifiers (nil refuses it), and
// Control records the exports in sage.auth_audit.
type WorkloadPackKeys struct {
	Signing   ed25519.PrivateKey
	Pseudonym []byte
	Control   *pgxpool.Pool
}

// packTier is a GET's tier and identifier hashing.
type packTier struct {
	tier string
	hash bool
}

func parsePackTier(r *http.Request) (packTier, error) {
	q := r.URL.Query()
	t := packTier{tier: q.Get("tier")}
	if t.tier == "" {
		t.tier = workpack.TierLocal
	}
	if t.tier != workpack.TierLocal && t.tier != workpack.TierModel {
		return t, fmt.Errorf("tier must be %q or %q", workpack.TierLocal, workpack.TierModel)
	}
	if raw := q.Get("hash_identifiers"); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil || (on && t.tier != workpack.TierModel) {
			return t, errors.New("hash_identifiers must be true or false, and only with " +
				"tier=model")
		}
		t.hash = on
	}
	return t, nil
}

// writeModelTier answers the model tier of pack (workpack.ViewOf) and,
// when hashed, the pseudonym map, which is local only and never part of
// the tier.
func writeModelTier(w http.ResponseWriter, row workpack.Stored, pack *workpack.Pack,
	t packTier, keys WorkloadPackKeys) {
	m, err := workpack.ForModel(pack, workpack.ModelOptions{HashIdentifiers: t.hash,
		PseudonymKey: keys.Pseudonym})
	if err != nil {
		modelTierError(w, err)
		return
	}
	out := struct {
		workpack.TierView
		Pack            workpack.Stored   `json:"pack"`
		HashIdentifiers bool              `json:"hash_identifiers"`
		Pseudonyms      map[string]string `json:"pseudonyms,omitempty"`
	}{TierView: workpack.ViewOf(m), Pack: row, HashIdentifiers: t.hash}
	if t.hash {
		out.Pseudonyms = m.Pseudonyms
	}
	jsonResponse(w, out)
}

func modelTierError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workpack.ErrNoPseudonymKey):
		sreErrorCode(w, err.Error(), "no_pseudonym_key", http.StatusConflict)
	case errors.Is(err, workpack.ErrModelLeak):
		sreErrorCode(w, err.Error(), "model_leak", http.StatusUnprocessableEntity)
	default:
		sreErrorCode(w, "derive the model tier: "+err.Error(), "model_tier_failed",
			http.StatusInternalServerError)
	}
}

func workloadPackExportHandler(mgr *fleet.DatabaseManager,
	keys WorkloadPackKeys) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		row, pack, ok := loadPack(w, r, mgr)
		if !ok {
			return
		}
		data, err := workpack.Export(pack, keys.Signing, time.Now())
		switch {
		case errors.Is(err, workpack.ErrNoSigningKey):
			sreErrorCode(w, err.Error(), "no_signing_key", http.StatusConflict)
			return
		case err != nil:
			internalError(w, r, "export workload pack "+row.ContentSHA256, err)
			return
		}
		db := r.PathValue("db")
		recordPackExport(r, keys.Control, db, row.ContentSHA256)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(
			`attachment; filename="pg_sage-workload-%s-%s.tar.gz"`, db,
			row.ContentSHA256[:12]))
		w.Header().Set("X-Evidence-Signed", "true")
		_, _ = w.Write(data)
	}
}

// recordPackExport audits who exported which workload pack.
func recordPackExport(r *http.Request, control *pgxpool.Pool, db, sha string) {
	actor := 0
	if u := UserFromContext(r.Context()); u != nil {
		actor = u.ID
	}
	recordAuthEvent(r, control, auth.AuthAuditEvent{Event: auditEvidenceExported,
		ActorUserID: actor, Detail: map[string]any{"database": db, "workload_pack": sha,
			"tier": workpack.TierExport}})
}
