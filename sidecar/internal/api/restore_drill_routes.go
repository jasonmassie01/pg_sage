package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/drill"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
)

// Recoverability (drill-01d), read-only:
//   - GET /api/v1/databases/{db}/restore-drills?limit=20 is one database's
//     newest drills (limit 1..100, default 20) with its status against the
//     35-day window, the backup isolation evidence (drill-01b) and the
//     cost label of the next full-copy drill.
//   - GET /api/v1/fleet/recoverability counts the databases the caller may
//     read by status, with the median RTO: the spec's "databases with a
//     passed restore drill in the last 35 days".
//
// Drill rows live where drills record them (the governance control
// database, else the database's own sage schema); the open
// recoverability_unknown finding lives in the database's own schema. A
// database whose history cannot be read counts as unknown, never passed.

const (
	restoreDrillsPath       = "/api/v1/databases/{db}/restore-drills"
	fleetRecoverabilityPath = "/api/v1/fleet/recoverability"
	drillReadTimeout        = 10 * time.Second
	noProviderReason        = "clone.provider is none"
)

// drillReads is what the routes read with: results names the pool a
// database's drill rows live in (nil, or a nil pool: the database's own);
// now is the clock (nil: time.Now).
type drillReads struct {
	cfg     *config.Config
	results func(database string) *pgxpool.Pool
	now     func() time.Time
}

// registerRestoreDrillRoutes registers the per-database route on the
// per-database mux (perDatabaseSREMux): "{db}/restore-drills" overlaps the
// fleet-mode "managed/{id}" routes on the API mux.
func registerRestoreDrillRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	reads drillReads) {
	route(mux, "GET "+restoreDrillsPath, gateViewer, restoreDrillsHandler(mgr, reads))
}

func registerFleetRecoverabilityRoute(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	reads drillReads) {
	routeAggregate(mux, "GET "+fleetRecoverabilityPath, gateViewer, mgr,
		func(m *fleet.DatabaseManager) http.Handler { return fleetRecoverabilityHandler(m, reads) })
}

func (d drillReads) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d drillReads) store(inst *fleet.DatabaseInstance) *drill.Store {
	if d.results != nil {
		if p := d.results(inst.Name); p != nil {
			return drill.NewStore(p)
		}
	}
	return drill.NewStore(inst.Pool)
}

func (d drillReads) cloneProvider() string {
	if d.cfg == nil {
		return ""
	}
	config.RLockForHotReload()
	defer config.RUnlockForHotReload()
	return d.cfg.Clone.Provider
}

// unknownReason is why no drill can run for a never-drilled database: its
// open recoverability_unknown finding, else no clone provider; "" when a
// drill can run.
func (d drillReads) unknownReason(ctx context.Context, inst *fleet.DatabaseInstance) (string,
	error) {
	reason, err := drill.UnknownReason(ctx, inst.Pool, inst.Name)
	if err != nil || reason != "" {
		return reason, err
	}
	if p := d.cloneProvider(); p == "" || p == "none" {
		return noProviderReason, nil
	}
	return "", nil
}

var errDrillNotConnected = errors.New("not connected")

// read is inst's newest limit drills and its recoverability at now.
func (d drillReads) read(ctx context.Context, inst *fleet.DatabaseInstance, limit int,
	now time.Time) ([]drill.Result, drill.Recoverability, error) {
	if inst.Pool == nil {
		return nil, drill.Recoverability{}, errDrillNotConnected
	}
	store := d.store(inst)
	history, err := store.History(ctx, inst.Name, limit)
	if err != nil {
		return nil, drill.Recoverability{}, err
	}
	lastPass, err := store.LastPassedResult(ctx, inst.Name)
	if err != nil {
		return nil, drill.Recoverability{}, err
	}
	var latest *drill.Result
	reason := ""
	if len(history) > 0 {
		latest = &history[0]
	} else if reason, err = d.unknownReason(ctx, inst); err != nil {
		return nil, drill.Recoverability{}, err
	}
	return history, drill.Classify(inst.Name, latest, lastPass, reason, now), nil
}

// drillIsolation is drill-01b's backup isolation evidence for the panel.
type drillIsolation struct {
	SurvivesDelete    *bool    `json:"survives_delete"`
	Evidence          string   `json:"evidence,omitempty"`
	Unavailable       string   `json:"unavailable,omitempty"`
	PITRWindowSeconds *float64 `json:"pitr_window_seconds"`
}

// isolationOf is the cloud telemetry's backup posture of database; nil
// when no cloud source reports one.
func isolationOf(database string) *drillIsolation {
	rt := cloudtel.Lookup(database)
	if rt == nil {
		return nil
	}
	b := rt.Backup()
	if b == nil {
		return nil
	}
	out := &drillIsolation{SurvivesDelete: b.SurvivesDelete, Evidence: b.IsolationEvidence,
		Unavailable: b.IsolationUnavailable}
	if b.PITRWindow != nil {
		s := b.PITRWindow.Seconds()
		out.PITRWindowSeconds = &s
	}
	return out
}

type restoreDrillsResponse struct {
	drill.Recoverability
	WindowDays int             `json:"window_days"`
	Drills     []drill.Result  `json:"drills"`
	Isolation  *drillIsolation `json:"isolation"`
	// NextFullCopyCost is the newest drill's cost label when it was a full
	// copy: the next one runs the same provider, kind and budget.
	NextFullCopyCost string `json:"next_full_copy_cost_label,omitempty"`
}

// parseDrillLimit reads ?limit= (default 20); false after a 400.
func parseDrillLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	q := r.URL.Query()
	if !q.Has("limit") {
		return drill.DefaultHistoryLimit, true
	}
	n, err := strconv.Atoi(q.Get("limit"))
	if err == nil {
		err = drill.ValidHistoryLimit(n)
	}
	if err != nil {
		jsonError(w, drill.ValidHistoryLimit(0).Error(), http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

func restoreDrillsHandler(mgr *fleet.DatabaseManager, reads drillReads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, ok := parseDrillLimit(w, r)
		if !ok {
			return
		}
		inst, ok := packInstance(w, r, mgr)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), drillReadTimeout)
		defer cancel()
		history, rec, err := reads.read(ctx, inst, limit, reads.clock())
		if err != nil {
			slog.Error("restore drill history read failed", "database", inst.Name,
				"error", err)
			jsonError(w, "the restore drills of "+inst.Name+" could not be read",
				http.StatusInternalServerError)
			return
		}
		out := restoreDrillsResponse{Recoverability: rec, Drills: history,
			WindowDays: int(drill.RecoverabilityWindow / (24 * time.Hour)),
			Isolation:  isolationOf(inst.Name)}
		if rec.Latest != nil && rec.Latest.Kind == clone.KindFullCopy {
			out.NextFullCopyCost = rec.Latest.CostLabel
		}
		jsonResponse(w, out)
	}
}

// fleetRecoverability counts names at now. A database that cannot be read
// is unknown and named in errors; the full error is logged only.
func fleetRecoverability(ctx context.Context, mgr *fleet.DatabaseManager, names []string,
	reads drillReads) drill.FleetRecoverability {
	now := reads.clock()
	list := make([]drill.Recoverability, 0, len(names))
	var errs []string
	for _, name := range names {
		var inst *fleet.DatabaseInstance
		if mgr != nil {
			inst = mgr.GetInstance(name)
		}
		if inst == nil {
			continue
		}
		_, rec, err := reads.read(ctx, inst, 1, now)
		if err != nil {
			why := "the drill history could not be read"
			if errors.Is(err, errDrillNotConnected) {
				why = "not connected"
			} else {
				slog.Error("restore drill history read failed", "database", name, "error", err)
			}
			rec = drill.Recoverability{Database: name, Status: drill.RecoverUnknown,
				Reason: why}
			errs = append(errs, "read restore drills: "+name+": "+why)
		}
		list = append(list, rec)
	}
	out := drill.Summarize(list)
	out.Errors = errs
	return out
}

func fleetRecoverabilityHandler(mgr *fleet.DatabaseManager, reads drillReads) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), drillReadTimeout)
		defer cancel()
		jsonResponse(w, fleetRecoverability(ctx, mgr, inboxDatabases(mgr, nil), reads))
	}
}

// RestoreDrillsMCP serves restore_drills from the same read as the fleet
// route; results is RuntimeDeps.RestoreDrillResults.
func RestoreDrillsMCP(mgr *fleet.DatabaseManager, cfg *config.Config,
	results func(string) *pgxpool.Pool) mcp.RestoreDrillsBackend {
	return restoreDrillsMCP{mgr: mgr, reads: drillReads{cfg: cfg, results: results}}
}

type restoreDrillsMCP struct {
	mgr   *fleet.DatabaseManager
	reads drillReads
}

func (b restoreDrillsMCP) RestoreDrills(ctx context.Context,
	req mcp.RestoreDrillsRequest) (any, error) {
	names := inboxDatabases(b.mgr, req.Databases)
	if req.Database != "" {
		if !contains(names, req.Database) {
			return nil, mcp.ErrRestoreDrillsDatabaseNotFound
		}
		names = []string{req.Database}
	}
	ctx, cancel := context.WithTimeout(ctx, drillReadTimeout)
	defer cancel()
	return fleetRecoverability(ctx, b.mgr, names, b.reads), nil
}
