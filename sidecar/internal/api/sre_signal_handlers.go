package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
	"github.com/pg-sage/sidecar/internal/sre/signed"
	"github.com/pg-sage/sidecar/internal/sre/slo"
)

// Sage SRE M5 machine ingestion (AI-SRE-SPEC §8/§9): signed change events
// and pushed SLI counters. No session: the HMAC signature over the
// timestamp, method, path and body is the authentication, inside the
// configured timestamp tolerance; replays are idempotent no-ops in the
// stores. Errors use canonical codes and never echo secrets.

const (
	changeEventsPath = "/api/v1/sre/change-events"
	sliPushPrefix    = "/api/v1/sre/sli/"
	maxSignedBody    = 16 << 10
)

func registerSRESignalRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager,
	cfg *config.Config) {
	viewerUp := RequireRole("admin", "operator", "viewer")
	mux.Handle("POST "+changeEventsPath, changeEventIngestHandler(mgr, cfg))
	mux.Handle("POST "+sliPushPrefix+"{name}", sliPushHandler(mgr, cfg))
	mux.Handle("GET /api/v1/sre/slos", viewerUp(sloListHandler(mgr)))
	mux.Handle("GET /api/v1/sre/slos/{name}", viewerUp(sloDetailHandler(mgr)))
	mux.Handle("GET /api/v1/sre/changes", viewerUp(changeListHandler(mgr)))
}

// isSignedIngestPath reports the routes authenticated by their signature
// instead of a session.
func isSignedIngestPath(path string) bool {
	if path == changeEventsPath {
		return true
	}
	name, ok := strings.CutPrefix(path, sliPushPrefix)
	return ok && name != "" && !strings.Contains(name, "/")
}

// readSigned reads a bounded body and verifies its signature; on failure
// it has written the response.
func readSigned(w http.ResponseWriter, r *http.Request, secret string,
	tolerance time.Duration) ([]byte, bool) {
	if secret == "" {
		sreErrorCode(w, "signed ingestion is not configured", "not_configured",
			http.StatusServiceUnavailable)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSignedBody+1))
	if err != nil || len(body) > maxSignedBody {
		sreErrorCode(w, "body too large or unreadable", "invalid_request",
			http.StatusRequestEntityTooLarge)
		return nil, false
	}
	_, err = signed.Verify([]byte(secret), signed.Request{Method: r.Method,
		Path: r.URL.Path, Timestamp: r.Header.Get(signed.HeaderTimestamp),
		Signature: r.Header.Get(signed.HeaderSignature), Body: body}, time.Now(), tolerance)
	switch {
	case err == nil:
		return body, true
	case errors.Is(err, signed.ErrMissing):
		sreErrorCode(w, "signature required", "missing_signature", http.StatusUnauthorized)
	case errors.Is(err, signed.ErrStale):
		sreErrorCode(w, "timestamp outside the tolerance", "stale_timestamp",
			http.StatusUnauthorized)
	default:
		sreErrorCode(w, "signature does not verify", "invalid_signature",
			http.StatusUnauthorized)
	}
	return nil, false
}

func changeEventIngestHandler(mgr *fleet.DatabaseManager, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ce := cfg.SRE.ChangeEvents
		body, ok := readSigned(w, r, ce.HMACSecret, ce.Tolerance())
		if !ok {
			return
		}
		var sub changefeed.Submission
		if err := json.Unmarshal(body, &sub); err != nil {
			sreErrorCode(w, "body must be a change event object", "invalid_request",
				http.StatusBadRequest)
			return
		}
		if len(ce.AllowedSources) > 0 && !slices.Contains(ce.AllowedSources, sub.Source) {
			sreErrorCode(w, "source is not allowed", "source_not_allowed", http.StatusForbidden)
			return
		}
		feeds, ok := targetFeeds(w, mgr, sub.Database)
		if !ok {
			return
		}
		ingestChange(w, r, feeds, sub)
	}
}

// targetFeeds is the named database's feed, or one feed per distinct
// store for a deployment-wide event.
func targetFeeds(w http.ResponseWriter, mgr *fleet.DatabaseManager,
	database string) ([]*changefeed.Feed, bool) {
	if database != "" {
		inst := instanceNamed(mgr, database)
		switch {
		case inst == nil:
			sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
			return nil, false
		case inst.Changes == nil:
			sreErrorCode(w, "change feed unavailable for this database",
				"metadata_unavailable", http.StatusServiceUnavailable)
			return nil, false
		}
		return []*changefeed.Feed{inst.Changes}, true
	}
	var feeds []*changefeed.Feed
	seen := map[any]bool{}
	for _, inst := range sortedInstances(mgr) {
		if inst.Changes != nil && !seen[inst.Changes.Store().Key()] {
			seen[inst.Changes.Store().Key()] = true
			feeds = append(feeds, inst.Changes)
		}
	}
	if len(feeds) == 0 {
		sreErrorCode(w, "change feed unavailable", "metadata_unavailable",
			http.StatusServiceUnavailable)
		return nil, false
	}
	return feeds, true
}

func ingestChange(w http.ResponseWriter, r *http.Request, feeds []*changefeed.Feed,
	sub changefeed.Submission) {
	var stored changefeed.Event
	created := false
	now := time.Now()
	for _, f := range feeds {
		e, c, err := f.Ingest(r.Context(), sub, now)
		if err != nil {
			signalError(w, r, err)
			return
		}
		stored, created = e, created || c
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": stored.ID, "duplicate": !created})
}

// signalError maps change-feed and SLO errors onto canonical codes.
func signalError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, changefeed.ErrInvalid), errors.Is(err, slo.ErrInvalidSample),
		errors.Is(err, sre.ErrInvalidRequest):
		sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
	case errors.Is(err, changefeed.ErrConflict):
		sreErrorCode(w, "event id already recorded with other content", "conflict",
			http.StatusConflict)
	case errors.Is(err, slo.ErrUnknownSLO), errors.Is(err, slo.ErrNotPush):
		sreErrorCode(w, "SLO not found", "not_found", http.StatusNotFound)
	case errors.Is(err, sre.ErrMetadataUnavailable):
		sreErrorCode(w, "SLO or change feed store unavailable", "metadata_unavailable",
			http.StatusServiceUnavailable)
	default:
		internalError(w, r, "sre signals", err)
	}
}

func sliPushHandler(mgr *fleet.DatabaseManager, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		push := cfg.SRE.SLO.Push
		body, ok := readSigned(w, r, push.HMACSecret, push.Tolerance())
		if !ok {
			return
		}
		name := r.PathValue("name")
		var p slo.PushSample
		if err := json.Unmarshal(body, &p); err != nil {
			sreErrorCode(w, "body must be a sample object", "invalid_request",
				http.StatusBadRequest)
			return
		}
		engines := pushEngines(mgr, name)
		if len(engines) == 0 {
			sreErrorCode(w, "SLO not found", "not_found", http.StatusNotFound)
			return
		}
		accepted := 0
		for _, e := range engines {
			created, err := e.Push(r.Context(), name, p)
			if err != nil {
				signalError(w, r, err)
				return
			}
			if created {
				accepted++
			}
		}
		status := http.StatusOK
		if accepted > 0 {
			status = http.StatusAccepted
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": accepted,
			"duplicate": accepted == 0})
	}
}

// pushEngines are the engines with the push SLO, one per distinct store.
func pushEngines(mgr *fleet.DatabaseManager, name string) []*slo.Engine {
	var out []*slo.Engine
	seen := map[any]bool{}
	for _, inst := range sortedInstances(mgr) {
		if inst.SLO == nil || !inst.SLO.HasPush(name) || seen[inst.SLO.Store().Key()] {
			continue
		}
		seen[inst.SLO.Store().Key()] = true
		out = append(out, inst.SLO)
	}
	return out
}

func instanceNamed(mgr *fleet.DatabaseManager, name string) *fleet.DatabaseInstance {
	if mgr == nil || name == "" || validateDatabaseParam(name) != nil {
		return nil
	}
	return mgr.GetInstance(name)
}

// sortedInstances lists the fleet's instances by name.
func sortedInstances(mgr *fleet.DatabaseManager) []*fleet.DatabaseInstance {
	if mgr == nil {
		return nil
	}
	out := make([]*fleet.DatabaseInstance, 0)
	for _, inst := range mgr.Instances() {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
