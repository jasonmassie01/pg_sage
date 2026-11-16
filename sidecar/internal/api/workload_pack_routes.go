package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/histstore"
)

// The workload evidence pack (evid-02a): an operator builds one for a
// database and window (POST, within packBuildTimeout); anyone who can read
// the database lists its packs, reads one pack's manifest and header, and
// resolves one evidence id. A build only reads the history and (evid-02b)
// the database's catalog, each read bounded by the safety timeouts; the
// pack is stored once per database and content hash. A viewer reads its
// model tier and an admin downloads its signed export (evid-02c,
// workload_pack_tiers.go).

const workloadPacksPath = "/api/v1/databases/{db}/workload-packs"

// packBuildTimeout bounds a build; packListLimit bounds a list.
const (
	packBuildTimeout = 60 * time.Second
	packListLimit    = 100
)

func registerWorkloadPackRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config, keys WorkloadPackKeys) {
	routeDB(mux, "POST "+workloadPacksPath, gateOperator, pathDB,
		workloadPackBuildHandler(mgr, cfg))
	route(mux, "GET "+workloadPacksPath, gateViewer, workloadPackListHandler(mgr))
	route(mux, "GET "+workloadPacksPath+"/{sha}", gateViewer,
		workloadPackGetHandler(mgr, keys))
	route(mux, "GET "+workloadPacksPath+"/{sha}/evidence/{id...}", gateViewer,
		workloadPackEvidenceHandler(mgr))
	route(mux, "GET "+workloadPacksPath+"/{sha}/export", gateAdmin,
		workloadPackExportHandler(mgr, keys))
}

// packInstance is the path's database with a pool, or false after an
// answer: 404 for an unknown database, 503 for one not connected.
func packInstance(w http.ResponseWriter, r *http.Request,
	mgr *fleet.DatabaseManager) (*fleet.DatabaseInstance, bool) {
	var inst *fleet.DatabaseInstance
	if mgr != nil {
		inst = mgr.GetInstance(r.PathValue("db"))
	}
	switch {
	case inst == nil:
		jsonError(w, "database not found", http.StatusNotFound)
		return nil, false
	case inst.Pool == nil:
		sreErrorCode(w, "the database is not connected", "database_unavailable",
			http.StatusServiceUnavailable)
		return nil, false
	}
	return inst, true
}

// packWindowBody is a build request; both bounds are optional (the 28
// days ending at the current hour). The two opt-ins (evid-02b, off by
// default) put plan conditions, literals redacted, and pg_stats' value
// samples into this one build's pack.
type packWindowBody struct {
	From                *time.Time `json:"from"`
	To                  *time.Time `json:"to"`
	IncludeConditions   bool       `json:"include_conditions"`
	IncludeValueSamples bool       `json:"include_value_samples"`
}

func parsePackWindow(r *http.Request, now time.Time) (workpack.Window, packWindowBody,
	error) {
	var b packWindowBody
	err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&b)
	if err != nil && !errors.Is(err, io.EOF) {
		return workpack.Window{}, b, fmt.Errorf("the body must be {\"from\", \"to\"} "+
			"with RFC 3339 times, and optional include_conditions and "+
			"include_value_samples booleans: %v", err)
	}
	w := workpack.DefaultWindowEnding(now)
	if b.To != nil {
		w = workpack.Window{From: b.To.Add(-workpack.DefaultWindow), To: *b.To}
	}
	if b.From != nil {
		w.From = *b.From
	}
	return w.UTC(), b, w.Validate()
}

// packRequest describes the build of w for inst.
func packRequest(inst *fleet.DatabaseInstance, cfg *config.Config, w workpack.Window,
	now time.Time) workpack.Request {
	req := workpack.Request{Database: inst.Name, Window: w, Now: now, Location: time.UTC,
		HourlyDays: config.DefaultRetentionQueryRollupHourlyDays}
	if cfg != nil {
		req.Location = cfg.Baseline.Location()
		req.HourlyDays = cfg.Retention.EffectiveQueryRollupHourlyDays()
		req.Timeouts = catalogread.FromSafety(cfg.Safety)
	}
	if rt := cloudtel.Lookup(inst.Name); rt != nil {
		req.Resource = rt.Resource()
	}
	if inst.Status != nil {
		req.PGVersion, req.Platform = inst.Status.PGVersion, inst.Status.Platform
	}
	if inst.Collector != nil {
		caps := inst.Collector.Capabilities()
		req.Capabilities = workpack.Capabilities{WALColumns: caps.WALColumns,
			PlanColumns: caps.PlanTimeColumns}
	}
	return req
}

func workloadPackBuildHandler(mgr *fleet.DatabaseManager, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()
		window, body, err := parsePackWindow(r, now)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), packBuildTimeout)
		defer cancel()
		req := packRequest(inst, cfg, window, now)
		req.IncludeConditions = body.IncludeConditions
		req.IncludeValueSamples = body.IncludeValueSamples
		row, err := buildAndSave(ctx, inst, req, r)
		if err != nil {
			packBuildError(w, r, inst.Name, err)
			return
		}
		status := http.StatusCreated
		if row.Reused {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(row)
	}
}

// buildAndSave builds and stores the pack, counting the result.
func buildAndSave(ctx context.Context, inst *fleet.DatabaseInstance,
	req workpack.Request, r *http.Request) (workpack.Stored, error) {
	builtBy := "unknown"
	if u := UserFromContext(r.Context()); u != nil && u.Email != "" {
		builtBy = u.Email
	}
	pack, err := workpack.Build(ctx, inst.Pool, req)
	if err == nil {
		var row workpack.Stored
		row, err = workpack.Save(ctx, inst.Pool, pack, req.Now, builtBy)
		if err == nil {
			result := workpack.ResultBuilt
			if row.Reused {
				result = workpack.ResultReused
			}
			workpack.NoteBuild(inst.Name, result, row.Bytes)
			return row, nil
		}
	}
	result := workpack.ResultFailed
	if errors.Is(err, workpack.ErrPackTooLarge) {
		result = workpack.ResultTooLarge
	}
	workpack.NoteBuild(inst.Name, result, 0)
	return workpack.Stored{}, err
}

func packBuildError(w http.ResponseWriter, r *http.Request, db string, err error) {
	switch {
	case errors.Is(err, workpack.ErrWindow):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, workpack.ErrPackTooLarge):
		sreErrorCode(w, err.Error(), "pack_too_large", http.StatusUnprocessableEntity)
	case errors.Is(err, histstore.ErrNotRegistered):
		sreErrorCode(w, "the history of "+db+" is not available to this runtime",
			"history_unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, context.DeadlineExceeded):
		sreErrorCode(w, "the build did not finish within 60 s", "build_timeout",
			http.StatusGatewayTimeout)
	default:
		internalError(w, r, "build the workload evidence pack of "+db, err)
	}
}
