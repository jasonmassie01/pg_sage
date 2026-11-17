package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/inbox"
	"github.com/pg-sage/sidecar/internal/store"
)

// Decision latency (ux-01a): a decision handler that settles an inbox item
// records how long the item had waited, labelled with the surface named
// by the X-Sage-Surface header (inbox, page, chat, api, cli; absent is
// api). The access log carries the same decided_from.

// recordDecision counts r's decision on an item of kind waiting since.
func recordDecision(r *http.Request, kind inbox.Kind, since time.Time) {
	if since.IsZero() {
		return
	}
	inbox.RecordDecision(kind, r.Header.Get(inbox.SurfaceHeader), time.Since(since))
}

// statusRecorder notes the status a handler answered.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// timeDecision wraps a decision route whose item since reads before h
// runs (zero: not waiting, nothing to time); a 2xx answer records it.
func timeDecision(kind inbox.Kind, since func(*http.Request) time.Time,
	h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		waiting := since(r)
		sw := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		if sw.status >= 200 && sw.status < 300 {
			recordDecision(r, kind, waiting)
		}
	})
}

// actionWaitingSince is when the {id} action was proposed, read from the
// store actions(r) names; zero unless it is still pending.
func actionWaitingSince(actions func(*http.Request) *store.ActionStore) func(
	*http.Request) time.Time {
	return func(r *http.Request) time.Time {
		id, err := strconv.Atoi(r.PathValue("id"))
		as := actions(r)
		if err != nil || as == nil {
			return time.Time{}
		}
		a, err := as.GetByID(r.Context(), id)
		if err != nil || a == nil || a.Status != "pending" {
			return time.Time{}
		}
		return a.ProposedAt
	}
}

// fixedActions is the standalone action store.
func fixedActions(as *store.ActionStore) func(*http.Request) *store.ActionStore {
	return func(*http.Request) *store.ActionStore { return as }
}

// fleetActions is the action store of ?database= (or the only database).
func fleetActions(mgr *fleet.DatabaseManager) func(*http.Request) *store.ActionStore {
	return func(r *http.Request) *store.ActionStore {
		name := oneDatabase(r.URL.Query().Get("database"))
		if name == "" {
			if all := sortedFleetInstances(mgr); len(all) == 1 {
				name = all[0].Name
			}
		}
		if inst := mgr.GetInstance(name); inst != nil && inst.Pool != nil {
			return store.NewActionStore(inst.Pool)
		}
		return nil
	}
}
