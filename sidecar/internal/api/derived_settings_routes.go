package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/selfconfig"
)

// Self-configuration (roadmap phase 3): GET /api/v1/derived-settings
// serves, per database (or ?database=), every derived key with its value,
// status (default, derived, shadow, pinned, operator), pending restart,
// cited evidence, bounds, rule and its newest ledger entries. Every
// signed-in role reads it. POST /api/v1/derived-settings/{key}/pin pins
// the value in force and .../unpin lets derivation resume (admin only).

const derivedSettingsPath = "/api/v1/derived-settings"

const derivedSettingsMeaning = "pg_sage derives these settings per database from " +
	"evidence (catalog size, scan times, connection limit, temp-file rate, its own " +
	"cost) when the operator leaves them unset, within bounds that never widen " +
	"authority or spend. A new value soaks in shadow and is promoted only when its " +
	"comparison with the active value is not worse; restart-bound keys take effect at " +
	"the next start. A value the operator set always wins; a pinned value ignores " +
	"new evidence until it is unpinned."

func registerDerivedSettingsRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewer := RequireRole("admin", "operator", "viewer")
	admin := RequireRole("admin")
	mux.Handle("GET "+derivedSettingsPath, viewer(derivedSettingsHandler(mgr)))
	mux.Handle("POST "+derivedSettingsPath+"/{key}/pin",
		admin(derivedSettingPinHandler(mgr, true)))
	mux.Handle("POST "+derivedSettingsPath+"/{key}/unpin",
		admin(derivedSettingPinHandler(mgr, false)))
}

type derivedShadowView struct {
	Value     float64   `json:"value"`
	Since     time.Time `json:"since"`
	Reason    string    `json:"reason"`
	Samples   int       `json:"samples"`
	SoakHours int       `json:"soak_hours"`
}

type derivedPinView struct {
	Value float64   `json:"value"`
	By    string    `json:"by"`
	At    time.Time `json:"at"`
}

type derivedSettingView struct {
	Key            string                   `json:"key"`
	Class          string                   `json:"class"`
	Lifecycle      string                   `json:"lifecycle"`
	Unit           string                   `json:"unit"`
	Summary        string                   `json:"summary"`
	Status         string                   `json:"status"`
	Value          float64                  `json:"value"`
	Default        float64                  `json:"default"`
	Active         *float64                 `json:"active"`
	Shadow         *derivedShadowView       `json:"shadow"`
	PendingRestart *float64                 `json:"pending_restart"`
	Pinned         *derivedPinView          `json:"pinned"`
	OperatorValue  *float64                 `json:"operator_value"`
	Note           string                   `json:"note"`
	Evidence       []selfconfig.Citation    `json:"evidence"`
	Bounds         selfconfig.Bounds        `json:"bounds"`
	Rule           string                   `json:"rule"`
	RuleVersion    int                      `json:"rule_version"`
	UpdatedAt      *time.Time               `json:"updated_at"`
	History        []selfconfig.LedgerEntry `json:"history"`
}

type derivedDatabaseView struct {
	Database string               `json:"database"`
	Settings []derivedSettingView `json:"settings"`
}

func derivedSettingsHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok || rejectUnknownDatabase(w, mgr, database) {
			return
		}
		out := []derivedDatabaseView{}
		for _, np := range poolsForDatabaseSelection(mgr, database) {
			settings, err := selfconfig.NewStore(np.pool).List(r.Context(), 10)
			if err != nil {
				internalError(w, r, "read derived settings of "+np.name, err)
				return
			}
			out = append(out, derivedDatabaseView{Database: np.name,
				Settings: derivedViews(settings, soakHours(mgr))})
		}
		jsonResponse(w, map[string]any{"databases": out, "meaning": derivedSettingsMeaning})
	}
}

// soakHours is the configured soak, for the shadow view.
func soakHours(mgr *fleet.DatabaseManager) int {
	if mgr != nil {
		if cfg := mgr.Config(); cfg != nil && cfg.SelfConfig.SoakHours > 0 {
			return cfg.SelfConfig.SoakHours
		}
	}
	return config.DefaultSelfConfigSoakHours
}

// derivedViews lists one view per rule: the stored state when there is
// one, otherwise the default with a note.
func derivedViews(settings []selfconfig.Setting, soak int) []derivedSettingView {
	byKey := map[string]selfconfig.Setting{}
	for _, s := range settings {
		byKey[s.Key] = s
	}
	def := config.DefaultConfig()
	out := make([]derivedSettingView, 0, len(selfconfig.Rules()))
	for _, rule := range selfconfig.Rules() {
		v := derivedSettingView{Key: rule.Key, Unit: rule.Unit, Summary: rule.Summary,
			Default: rule.Get(def), Rule: rule.Name, RuleVersion: rule.Version,
			Lifecycle: lifecycleOf(rule.Key), Status: string(selfconfig.StatusDefault),
			Value: rule.Get(def), Bounds: rule.Bounds(def),
			Evidence: []selfconfig.Citation{}, History: []selfconfig.LedgerEntry{},
			Note: "not derived yet on this database"}
		if class, ok := config.KeyClassOf(rule.Key); ok {
			v.Class = string(class)
		}
		if s, ok := byKey[rule.Key]; ok {
			fillDerivedView(&v, s, soak)
		}
		out = append(out, v)
	}
	return out
}

func fillDerivedView(v *derivedSettingView, s selfconfig.Setting, soak int) {
	v.Status, v.Value, v.Active = string(s.Status), s.Value, s.Active
	v.PendingRestart, v.OperatorValue, v.Note = s.Pending, s.Operator, s.Note
	v.Bounds, v.RuleVersion, v.Rule = s.Bounds, s.RuleVersion, s.Rule
	if s.Evidence != nil {
		v.Evidence = s.Evidence
	}
	if s.History != nil {
		v.History = s.History
	}
	if !s.UpdatedAt.IsZero() {
		at := s.UpdatedAt
		v.UpdatedAt = &at
	}
	if s.Shadow != nil {
		v.Shadow = &derivedShadowView{Value: *s.Shadow, Since: s.ShadowSince,
			Reason: s.ShadowReason, Samples: len(s.Samples), SoakHours: soak}
	}
	if s.Pinned != nil {
		v.Pinned = &derivedPinView{Value: *s.Pinned, By: s.PinnedBy, At: s.PinnedAt}
	}
}

func lifecycleOf(key string) string {
	if f, ok := config.LookupFieldLifecycle(key); ok {
		return string(f.Lifecycle)
	}
	return string(config.LifecycleRestart)
}

func derivedSettingPinHandler(mgr *fleet.DatabaseManager, pin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if _, ok := selfconfig.Lookup(key); !ok {
			jsonError(w, "not a derived setting", http.StatusNotFound)
			return
		}
		database, ok := readDatabaseParam(w, r)
		if !ok || rejectUnknownDatabase(w, mgr, database) {
			return
		}
		target, ok := resolveSingleDatabaseRequestPool(mgr, database)
		if !ok || target.pool == nil {
			jsonError(w, "name one database with ?database=", http.StatusBadRequest)
			return
		}
		actor := "admin"
		if user := UserFromContext(r.Context()); user != nil && user.Email != "" {
			actor = user.Email
		}
		store := selfconfig.NewStore(target.pool)
		change := store.Unpin
		if pin {
			change = store.Pin
		}
		if _, err := change(r.Context(), key, actor); err != nil {
			writePinError(w, r, err)
			return
		}
		settings, err := store.List(r.Context(), 10)
		if err != nil {
			internalError(w, r, "read derived settings of "+target.name, err)
			return
		}
		for _, v := range derivedViews(settings, soakHours(mgr)) {
			if v.Key == key {
				jsonResponse(w, v)
				return
			}
		}
		jsonError(w, "setting vanished after the change", http.StatusInternalServerError)
	}
}

func writePinError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, selfconfig.ErrNotFound):
		jsonError(w, "not derived yet on this database; nothing to pin",
			http.StatusConflict)
	case errors.Is(err, selfconfig.ErrOperatorSet):
		jsonError(w, "set by the operator in the configuration: change it there",
			http.StatusConflict)
	case errors.Is(err, selfconfig.ErrNotPinned):
		jsonError(w, "not pinned", http.StatusConflict)
	default:
		internalError(w, r, "change the pin of a derived setting", err)
	}
}
