package api

import (
	"net/http"
	"sort"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfbudget"
	"github.com/pg-sage/sidecar/internal/selfcost"
)

// pg_sage's own footprint (footprint-08): GET /api/v1/self-cost serves,
// per database (or ?database=), what pg_sage's own statements cost it per
// hour against the self-budget, the cost per component (from the tag
// that names the sender), the costliest statements and the sage schema's
// size; the sidecar process's CPU and busiest loops appear once. Every
// signed-in role reads it. A figure not measured yet is null, never 0.

const selfCostPath = "/api/v1/self-cost"

const selfCostMeaning = "What pg_sage itself costs each database it monitors: the " +
	"database time, calls and shared blocks of its own statements (pg_stat_statements " +
	"entries tagged /* pg_sage */) and the sage-table rows it reads and writes, per hour " +
	"over the analyzer's latest window, against the self-budget; by_component splits the " +
	"time by the component its tag names (unattributed: entries pg_stat_statements first " +
	"saw before the tag named one). A null figure is not measured yet (first cycle, no " +
	"pg_stat_statements); a null budget is not set. CPU is the sidecar process's, for all " +
	"databases together."

// registerSelfAwareRoutes serves what pg_sage knows about itself: shadow
// mode, its derived settings and its own footprint.
func registerSelfAwareRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	registerShadowRoutes(mux, mgr)
	registerDerivedSettingsRoutes(mux, mgr)
	registerSelfCostRoutes(mux, mgr)
}

func registerSelfCostRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	routeAggregate(mux, "GET "+selfCostPath, gateViewer, mgr, aggregateHandler(selfCostHandler))
}

type footprintKnown struct {
	Window     bool `json:"window"`
	Statements bool `json:"statements"`
	Storage    bool `json:"storage"`
}

type footprintStatement struct {
	Text      string  `json:"text"`
	Component string  `json:"component"`
	DBTimeMs  float64 `json:"db_time_ms"`
	Calls     int64   `json:"calls"`
	Blocks    int64   `json:"blocks"`
}

type footprintComponent struct {
	Component       string  `json:"component"`
	DBTimeMsPerHour float64 `json:"db_time_ms_per_hour"`
	CallsPerHour    float64 `json:"calls_per_hour"`
	BlocksPerHour   float64 `json:"blocks_per_hour"`
	Share           float64 `json:"share"`
}

type footprintDatabaseView struct {
	Database           string               `json:"database"`
	Known              footprintKnown       `json:"known"`
	WindowSeconds      *float64             `json:"window_seconds"`
	CycleSeconds       *float64             `json:"cycle_seconds"`
	DBTimeMsPerHour    *float64             `json:"db_time_ms_per_hour"`
	DBTimeBudget       *float64             `json:"db_time_budget_ms_per_hour"`
	DBTimeBudgetSource string               `json:"db_time_budget_source"`
	CallsPerHour       *float64             `json:"calls_per_hour"`
	BlocksPerHour      *float64             `json:"blocks_per_hour"`
	BlocksBudget       *float64             `json:"blocks_budget_per_hour"`
	RowsReadPerHour    *float64             `json:"rows_read_per_hour"`
	RowsWrittenPerHour *float64             `json:"rows_written_per_hour"`
	SchemaBytes        *float64             `json:"schema_bytes"`
	StorageBudgetBytes *float64             `json:"storage_budget_bytes"`
	UnattributedShare  *float64             `json:"unattributed_share"`
	TopStatements      []footprintStatement `json:"top_statements"`
	ByComponent        []footprintComponent `json:"by_component"`
}

type footprintLoop struct {
	Loop   string  `json:"loop"`
	BusyMs float64 `json:"busy_ms"`
	Runs   int64   `json:"runs"`
}

type footprintProcessView struct {
	Known     bool            `json:"known"`
	CPUMs     *float64        `json:"cpu_ms_per_cycle"`
	CPUBudget *float64        `json:"cpu_budget_ms_per_cycle"`
	TopLoops  []footprintLoop `json:"top_loops"`
}

func selfCostHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok || rejectUnknownDatabase(w, mgr, database) {
			return
		}
		cfg := config.DefaultConfig()
		if mgr != nil && mgr.Config() != nil {
			cfg = mgr.Config()
		}
		names, footprints := selectedFootprints(mgr, database)
		views := make([]footprintDatabaseView, 0, len(names))
		for _, name := range names {
			views = append(views, footprintView(name, footprints[name], cfg))
		}
		jsonResponse(w, map[string]any{"databases": views,
			"process": processView(selfbudget.ProcessWatch().Reading(), cfg),
			"meaning": selfCostMeaning})
	}
}

// selectedFootprints is the sorted database names of the selection and
// each one's latest footprint (empty for an instance with no analyzer).
func selectedFootprints(mgr *fleet.DatabaseManager, database string) ([]string,
	map[string]analyzer.SelfFootprint) {
	out := map[string]analyzer.SelfFootprint{}
	if mgr == nil {
		return nil, out
	}
	instances := mgr.Instances()
	if database != "" && database != "all" {
		instances = map[string]*fleet.DatabaseInstance{database: mgr.GetInstance(database)}
	}
	names := make([]string, 0, len(instances))
	for name, inst := range instances {
		names = append(names, name)
		if inst != nil {
			out[name] = inst.Analyzer.SelfFootprint()
		}
	}
	sort.Strings(names)
	return names, out
}

func known(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}

// footprintView is one database's figures: rates per hour once a window
// is known, the database-time rates only with pg_stat_statements on both
// sides, the size once read; null otherwise.
func footprintView(name string, fp analyzer.SelfFootprint,
	cfg *config.Config) footprintDatabaseView {
	c := fp.Cost
	cycle := c.CycleSeconds > 0
	window := c.Known && cycle
	stmts := window && c.DBTimeKnown
	perHour := 0.0
	if cycle {
		perHour = 3600 / c.CycleSeconds
	}
	storage := c.SchemaBytes > 0
	v := footprintDatabaseView{Database: name,
		Known:              footprintKnown{Window: window, Statements: stmts, Storage: storage},
		WindowSeconds:      known(c.WindowSeconds, window),
		CycleSeconds:       known(c.CycleSeconds, cycle),
		DBTimeMsPerHour:    known(c.DBTimeMsPerCycle*perHour, stmts),
		CallsPerHour:       known(c.CallsPerCycle*perHour, stmts),
		BlocksPerHour:      known(c.BlocksPerCycle*perHour, stmts),
		RowsReadPerHour:    known(c.RowsReadPerCycle*perHour, window),
		RowsWrittenPerHour: known(c.RowsWrittenPerCycle*perHour, window),
		SchemaBytes:        known(float64(c.SchemaBytes), storage),
		BlocksBudget: known(float64(cfg.SelfBudget.BlocksPerHour),
			cfg.SelfBudget.BlocksPerHour > 0),
		StorageBudgetBytes: known(float64(int64(cfg.SelfBudget.StorageMB)<<20),
			cfg.SelfBudget.StorageMB > 0),
		TopStatements: footprintStatements(fp.Top, stmts),
	}
	v.DBTimeBudget, v.DBTimeBudgetSource = dbTimeBudget(cfg, c.CycleSeconds)
	v.ByComponent, v.UnattributedShare = footprintComponents(fp.ByComponent, stmts)
	return v
}

// dbTimeBudget is the database-time budget per hour on a database whose
// collector runs every cycleSeconds and where it comes from: an explicit
// self_budget.db_time_ms_per_hour, or analyzer.self_cost_budget_ms at
// that period (null while the period is unknown), or none.
func dbTimeBudget(cfg *config.Config, cycleSeconds float64) (*float64, string) {
	if cfg.SelfBudget.DBTimeMsPerHour > 0 {
		return known(float64(cfg.SelfBudget.DBTimeMsPerHour), true),
			"self_budget.db_time_ms_per_hour"
	}
	if cfg.Analyzer.SelfCostBudgetMs <= 0 {
		return nil, "none"
	}
	period := time.Duration(cycleSeconds * float64(time.Second))
	limit := config.InheritedDBTimeMsPerHour(cfg.Analyzer.SelfCostBudgetMs, period)
	return known(limit, limit > 0), "analyzer.self_cost_budget_ms"
}

func footprintStatements(top []selfcost.StatementCost, ok bool) []footprintStatement {
	out := []footprintStatement{}
	if !ok {
		return out
	}
	for _, s := range top {
		out = append(out, footprintStatement{Text: s.Text, Component: s.Component,
			DBTimeMs: s.TimeMs, Calls: s.Calls, Blocks: s.Blocks})
	}
	return out
}

// footprintComponents are the per-component rows with their share of the
// window's database time, and the unattributed share (0 when every
// component is named, null without a breakdown).
func footprintComponents(comps []selfcost.ComponentCost, ok bool) ([]footprintComponent,
	*float64) {
	out := []footprintComponent{}
	if !ok {
		return out, nil
	}
	total := 0.0
	for _, c := range comps {
		total += c.DBTimeMsPerHour
	}
	unattributed := 0.0
	for _, c := range comps {
		share := 0.0
		if total > 0 {
			share = c.DBTimeMsPerHour / total
		}
		if c.Component == selfcost.Unattributed {
			unattributed = share
		}
		out = append(out, footprintComponent{Component: c.Component,
			DBTimeMsPerHour: c.DBTimeMsPerHour, CallsPerHour: c.CallsPerHour,
			BlocksPerHour: c.BlocksPerHour, Share: share})
	}
	return out, &unattributed
}

// processView is the sidecar process's CPU per cycle against its budget
// and its busiest loops of local work, once for all databases, as the
// process watch measured them (footprint-07).
func processView(r selfbudget.CPUReading, cfg *config.Config) footprintProcessView {
	budget := float64(cfg.SelfBudget.CPUMsPerCycle)
	v := footprintProcessView{TopLoops: []footprintLoop{}, CPUBudget: known(budget, budget > 0)}
	if !r.Known {
		return v
	}
	v.Known, v.CPUMs = true, known(r.MsPerCycle, true)
	for _, l := range r.Loops {
		v.TopLoops = append(v.TopLoops, footprintLoop{Loop: l.Name, BusyMs: l.BusyMs,
			Runs: l.Runs})
	}
	return v
}
