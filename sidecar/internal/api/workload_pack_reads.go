package api

import (
	"errors"
	"net/http"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// The read routes of the workload evidence pack (evid-02a).

func workloadPackListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		rows, err := workpack.List(r.Context(), inst.Pool, packListLimit)
		if err != nil {
			packReadError(w, r, inst.Name, err)
			return
		}
		if rows == nil {
			rows = []workpack.Stored{}
		}
		jsonResponse(w, map[string]any{"database": inst.Name, "packs": rows})
	}
}

// loadPack is the path's stored pack, or false after an answer.
func loadPack(w http.ResponseWriter, r *http.Request,
	mgr *fleet.DatabaseManager) (workpack.Stored, *workpack.Pack, bool) {
	sha := r.PathValue("sha")
	if !workpack.ValidSHA(sha) {
		jsonError(w, "the pack hash must be 64 lower-case hex digits", http.StatusBadRequest)
		return workpack.Stored{}, nil, false
	}
	inst, ok := packInstance(w, r, mgr)
	if !ok {
		return workpack.Stored{}, nil, false
	}
	row, pack, err := workpack.Load(r.Context(), inst.Pool, sha)
	if err != nil {
		packReadError(w, r, inst.Name, err)
		return workpack.Stored{}, nil, false
	}
	return row, pack, true
}

func workloadPackGetHandler(mgr *fleet.DatabaseManager,
	keys WorkloadPackKeys) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tier, err := parsePackTier(r)
		if err != nil {
			sreErrorCode(w, err.Error(), "invalid_tier", http.StatusBadRequest)
			return
		}
		row, pack, ok := loadPack(w, r, mgr)
		if !ok {
			return
		}
		if tier.tier == workpack.TierModel {
			writeModelTier(w, row, pack, tier, keys)
			return
		}
		hdr, err := pack.Header()
		if err != nil {
			internalError(w, r, "read the header of workload pack "+row.ContentSHA256, err)
			return
		}
		jsonResponse(w, map[string]any{"pack": row, "manifest": pack.Manifest,
			"header": hdr, "evidence_ids": pack.IDs()})
	}
}

func workloadPackEvidenceHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, pack, ok := loadPack(w, r, mgr)
		if !ok {
			return
		}
		fact, err := workpack.Resolve(pack, r.PathValue("id"))
		if err != nil {
			sreErrorCode(w, err.Error(), "unknown_evidence", http.StatusNotFound)
			return
		}
		jsonResponse(w, fact)
	}
}

func packReadError(w http.ResponseWriter, r *http.Request, db string, err error) {
	switch {
	case errors.Is(err, workpack.ErrNotFound):
		sreErrorCode(w, "workload pack not found", "not_found", http.StatusNotFound)
	default:
		packBuildError(w, r, db, err)
	}
}
