package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// This is an ordinary human-admin operation behind platform authentication and
// same-origin mutation protection. It grants no machine/source or agent identity
// and deliberately does not enable the separate signed ingestion protocols.
func registerAppSLISampleRoute(mux *http.ServeMux, mgr *fleet.DatabaseManager, cfg *config.Config) {
	mux.Handle("POST /api/v1/sre/slos/{name}/samples", RequireRole(auth.RoleAdmin)(appSLISampleHandler(mgr, cfg)))
}

func appSLISampleHandler(mgr *fleet.DatabaseManager, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		mode, _ := r.Context().Value(authModeContextKey).(string)
		if user == nil || user.ID <= 0 || user.Role != auth.RoleAdmin || mode != auth.DatabricksAppMode {
			sreErrorCode(w, "Databricks administrator identity required", "forbidden", http.StatusForbidden)
			return
		}
		if cfg == nil || !cfg.SRE.SLO.Enabled {
			sreErrorCode(w, "SLO ingestion is not enabled", "not_configured", http.StatusServiceUnavailable)
			return
		}
		names, databases := r.URL.Query()["name"], r.URL.Query()["database"]
		if len(names) != 0 || len(databases) != 1 || databases[0] == "" {
			sreErrorCode(w, "one explicit database is required", "invalid_request", http.StatusBadRequest)
			return
		}
		name, database := r.PathValue("name"), databases[0]
		inst := instanceNamed(mgr, database)
		if inst == nil {
			sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
			return
		}
		configured := false
		for _, o := range cfg.SRE.SLO.Objectives {
			if o.Name == name && o.Database == inst.Name && o.Source == "push" {
				configured = true
			}
		}
		if !configured || inst.SLO == nil || !inst.SLO.DatabaseScopedPush() || !inst.SLO.HasPush(name) || inst.SLO.Database() != inst.Name {
			sreErrorCode(w, "explicit database-bound push SLO not found", "not_found", http.StatusNotFound)
			return
		}
		// The durable samples key predates database scopes. Fail closed if a
		// registered second engine could consume the same deployment SLO name.
		for _, other := range sortedInstances(mgr) {
			if other.Name != inst.Name && other.SLO != nil && other.SLO.HasPush(name) {
				sreErrorCode(w, "SLO name is ambiguous across databases", "scope_ambiguous", http.StatusConflict)
				return
			}
		}
		if inst.Investigations == nil || inst.Investigations.Coordinator() == nil {
			sreErrorCode(w, "registered database identity unavailable", "metadata_unavailable", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSignedBody+1))
		if err != nil || len(body) > maxSignedBody {
			sreErrorCode(w, "body too large or unreadable", "invalid_request", http.StatusRequestEntityTooLarge)
			return
		}
		var p slo.PushSample
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&p); err != nil || dec.Decode(new(any)) != io.EOF {
			sreErrorCode(w, "body must contain only a sample object", "invalid_request", http.StatusBadRequest)
			return
		}
		if p.Series == "" {
			p.Series = slo.DefaultSeries
		}
		p.Series = fmt.Sprintf("databricks_user:%d:%s", user.ID, p.Series)
		p.ObservedAt = p.ObservedAt.UTC().Truncate(time.Microsecond)
		scope, err := inst.Investigations.Coordinator().Bind(r.Context())
		if err != nil {
			signalError(w, r, err)
			return
		}
		created, err := inst.SLO.PushScoped(r.Context(), scope, name, p)
		if err != nil {
			signalError(w, r, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusAccepted
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": created, "duplicate": !created,
			"database": inst.Name, "slo": name})
	}
}
