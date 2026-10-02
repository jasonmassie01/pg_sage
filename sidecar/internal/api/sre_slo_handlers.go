package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// Sage SRE M5 reads (any signed-in role): SLO error-budget state with
// burn rates and unknown reasons, one SLO with its history and the SLI
// recovery predicate, and the change feed.

// Change-feed read bounds.
const (
	defaultChangeWindowMinutes = 60
	maxChangeWindowMinutes     = 7 * 24 * 60
	changeListLimit            = 200
	sloTransitionLimit         = 50
)

func sloListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		insts, ok := signalInstances(w, mgr, r.URL.Query().Get("database"))
		if !ok {
			return
		}
		items := []slo.Status{}
		unavailable := []string{}
		for _, inst := range insts {
			if inst.SLO == nil {
				continue
			}
			sts, err := inst.SLO.Statuses(r.Context())
			if err != nil {
				unavailable = append(unavailable, inst.Name)
				continue
			}
			items = append(items, sts...)
		}
		jsonResponse(w, map[string]any{"slos": items, "unavailable": unavailable})
	}
}

// signalInstances is the named database, or every database.
func signalInstances(w http.ResponseWriter, mgr *fleet.DatabaseManager,
	database string) ([]*fleet.DatabaseInstance, bool) {
	if database == "" {
		return sortedInstances(mgr), true
	}
	inst := instanceNamed(mgr, database)
	if inst == nil {
		sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
		return nil, false
	}
	return []*fleet.DatabaseInstance{inst}, true
}

// sloEngine finds the engine of a named SLO: the named database's, or
// the only database that has it.
func sloEngine(w http.ResponseWriter, mgr *fleet.DatabaseManager, database,
	name string) (*slo.Engine, bool) {
	insts, ok := signalInstances(w, mgr, database)
	if !ok {
		return nil, false
	}
	var found []*slo.Engine
	for _, inst := range insts {
		if inst.SLO == nil {
			continue
		}
		for _, o := range inst.SLO.Objectives() {
			if o.Name == name {
				found = append(found, inst.SLO)
			}
		}
	}
	switch len(found) {
	case 0:
		sreErrorCode(w, "SLO not found", "not_found", http.StatusNotFound)
		return nil, false
	case 1:
		return found[0], true
	}
	sreErrorCode(w, fmt.Sprintf("%d databases have this SLO: name the database",
		len(found)), "invalid_request", http.StatusBadRequest)
	return nil, false
}

func sloDetailHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var since time.Time
		if raw := q.Get("since"); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				sreErrorCode(w, "since must be an RFC 3339 time", "invalid_request",
					http.StatusBadRequest)
				return
			}
			since = t
		}
		name := r.PathValue("name")
		e, ok := sloEngine(w, mgr, q.Get("database"), name)
		if !ok {
			return
		}
		st, err := e.Status(r.Context(), name)
		if err != nil {
			signalError(w, r, err)
			return
		}
		trs, err := e.Transitions(r.Context(), name, sloTransitionLimit)
		if err != nil {
			signalError(w, r, err)
			return
		}
		out := map[string]any{"database": e.Database(), "status": st, "transitions": trs}
		if !since.IsZero() {
			rec, err := e.Recovery(r.Context(), name, since)
			if err != nil {
				signalError(w, r, err)
				return
			}
			out["recovery"] = rec
		}
		jsonResponse(w, out)
	}
}

func changeListHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		minutes := defaultChangeWindowMinutes
		if raw := q.Get("window_minutes"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > maxChangeWindowMinutes {
				sreErrorCode(w, fmt.Sprintf("window_minutes must be 1-%d",
					maxChangeWindowMinutes), "invalid_request", http.StatusBadRequest)
				return
			}
			minutes = n
		}
		inst := instanceNamed(mgr, q.Get("database"))
		if inst == nil {
			sreErrorCode(w, "database not found (name it with ?database=)", "not_found",
				http.StatusNotFound)
			return
		}
		if inst.Changes == nil {
			sreErrorCode(w, "change feed unavailable for this database",
				"metadata_unavailable", http.StatusServiceUnavailable)
			return
		}
		evs, err := inst.Changes.Recent(r.Context(), time.Duration(minutes)*time.Minute,
			changeListLimit)
		if err != nil {
			signalError(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"database": inst.Name, "window_minutes": minutes,
			"changes": evs})
	}
}
