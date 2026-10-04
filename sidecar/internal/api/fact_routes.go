package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Binding facts (roadmap 2.3). GET /api/v1/facts lists a database's facts
// (?status=a,b, ?type=); POST /api/v1/facts declares an operator's fact,
// confirmed by the declaration; POST /api/v1/facts/{id}/confirm|reject|
// expire decides one; GET /api/v1/facts/match?object=... answers which
// facts are about given objects (findings, approval cards). Every
// signed-in role reads; operators and admins decide. Confirmed facts only
// narrow or redirect what pg_sage does.

const (
	maxFactBody     = 16 << 10
	maxMatchObjects = 50
)

type factHandlers struct{ mgr *fleet.DatabaseManager }

func registerFactRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewer := RequireRole("admin", "operator", "viewer")
	operatorUp := RequireRole("admin", "operator")
	h := factHandlers{mgr: mgr}
	mux.Handle("GET /api/v1/facts", viewer(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/v1/facts/match", viewer(http.HandlerFunc(h.match)))
	mux.Handle("POST /api/v1/facts", operatorUp(http.HandlerFunc(h.declare)))
	for _, verb := range []string{"confirm", "reject", "expire"} {
		mux.Handle("POST /api/v1/facts/{id}/"+verb, operatorUp(h.decide(verb)))
	}
}

// factView is a fact as served: with its database, its words and who
// decided it.
type factView struct {
	facts.Fact
	Database   string `json:"database"`
	Summary    string `json:"summary"`
	Provenance string `json:"provenance"`
}

func viewOf(database string, f facts.Fact) factView {
	return factView{Fact: f, Database: database, Summary: f.Describe(),
		Provenance: f.Provenance()}
}

func (h factHandlers) list(w http.ResponseWriter, r *http.Request) {
	db, ok := readDatabaseParam(w, r)
	if !ok || rejectUnknownDatabase(w, h.mgr, db) {
		return
	}
	filter := facts.Filter{Type: facts.Type(r.URL.Query().Get("type"))}
	for _, st := range strings.Split(r.URL.Query().Get("status"), ",") {
		if st = strings.TrimSpace(st); st != "" {
			filter.Status = append(filter.Status, facts.Status(st))
		}
	}
	out := []factView{}
	failures := make([]map[string]string, 0)
	for _, np := range poolsForDatabaseSelection(h.mgr, db) {
		got, err := facts.NewStore(np.pool).List(r.Context(), filter)
		if errors.Is(err, facts.ErrInvalidValue) {
			sreErrorCode(w, err.Error(), "invalid_request", http.StatusBadRequest)
			return
		}
		if err != nil {
			failures = append(failures, fleetReadFailure(np.name, "list facts", err))
			continue
		}
		for _, f := range got {
			out = append(out, viewOf(np.name, f))
		}
	}
	jsonResponse(w, map[string]any{"facts": out, "total": len(out), "errors": failures})
}

// target resolves ?database= to one monitored database (optional with one).
func (h factHandlers) target(w http.ResponseWriter, r *http.Request) (
	*fleet.DatabaseInstance, bool) {
	db, ok := readDatabaseParam(w, r)
	if !ok {
		return nil, false
	}
	if db == "" || db == "all" {
		instances := sortedFleetInstances(h.mgr)
		if len(instances) != 1 {
			sreErrorCode(w, "database is required", "invalid_request", http.StatusBadRequest)
			return nil, false
		}
		db = instances[0].Name
	}
	inst := h.mgr.GetInstance(db)
	if inst == nil || inst.Pool == nil {
		sreErrorCode(w, "database not found", "not_found", http.StatusNotFound)
		return nil, false
	}
	return inst, true
}

type declareBody struct {
	Type        string            `json:"type"`
	SubjectKind string            `json:"subject_kind"`
	Subject     string            `json:"subject"`
	Value       map[string]string `json:"value"`
	Note        string            `json:"note"`
	ExpiresAt   *time.Time        `json:"expires_at"`
}

func (h factHandlers) declare(w http.ResponseWriter, r *http.Request) {
	inst, ok := h.target(w, r)
	if !ok {
		return
	}
	var body declareBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFactBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		sreErrorCode(w, "invalid fact body", "invalid_request", http.StatusBadRequest)
		return
	}
	f, err := facts.NewStore(inst.Pool).Declare(r.Context(), facts.Proposal{
		Type: facts.Type(body.Type), Kind: facts.Kind(body.SubjectKind),
		Subject: body.Subject, Value: body.Value, ExpiresAt: body.ExpiresAt,
		Rationale: body.Note}, factActor(r), body.Note)
	if err != nil {
		writeFactError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"fact": viewOf(inst.Name, f)})
}

func (h factHandlers) decide(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			sreErrorCode(w, "invalid fact id", "invalid_request", http.StatusBadRequest)
			return
		}
		inst, ok := h.target(w, r)
		if !ok {
			return
		}
		var body struct {
			Note string `json:"note"`
		}
		if r.ContentLength != 0 {
			dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFactBody))
			if err := dec.Decode(&body); err != nil {
				sreErrorCode(w, "invalid body", "invalid_request", http.StatusBadRequest)
				return
			}
		}
		f, err := decideFact(r, facts.NewStore(inst.Pool), id, verb, body.Note)
		if err != nil {
			writeFactError(w, r, err)
			return
		}
		jsonResponse(w, map[string]any{"fact": viewOf(inst.Name, f)})
	}
}

func decideFact(r *http.Request, s *facts.Store, id int64, verb, note string) (facts.Fact,
	error) {
	if verb == "expire" {
		reason := "expired by " + factActor(r)
		if strings.TrimSpace(note) != "" {
			reason += ": " + note
		}
		return s.Expire(r.Context(), id, reason)
	}
	return s.Decide(r.Context(), id, facts.Decision{Confirm: verb == "confirm",
		Actor: factActor(r), Note: note})
}

// factActor names the signed-in user for the fact's audit fields.
func factActor(r *http.Request) string {
	user := UserFromContext(r.Context())
	if user == nil {
		return "unknown"
	}
	if user.Email != "" {
		return user.Email
	}
	return fmt.Sprintf("user:%d", user.ID)
}

func (h factHandlers) match(w http.ResponseWriter, r *http.Request) {
	inst, ok := h.target(w, r)
	if !ok {
		return
	}
	objects := r.URL.Query()["object"]
	if len(objects) == 0 || len(objects) > maxMatchObjects {
		sreErrorCode(w, fmt.Sprintf("give 1-%d objects", maxMatchObjects),
			"invalid_request", http.StatusBadRequest)
		return
	}
	all, err := facts.NewStore(inst.Pool).List(r.Context(), facts.Filter{
		Status: []facts.Status{facts.StatusProposed, facts.StatusConfirmed}})
	if err != nil {
		internalError(w, r, "match facts", err)
		return
	}
	out := make(map[string]map[string][]factView, len(objects))
	for _, object := range objects {
		entry := map[string][]factView{"confirmed": {}, "proposed": {}}
		for _, f := range facts.Matching(all, object) {
			entry[string(f.Status)] = append(entry[string(f.Status)], viewOf(inst.Name, f))
		}
		out[object] = entry
	}
	jsonResponse(w, map[string]any{"database": inst.Name, "objects": out})
}

func writeFactError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, facts.ErrProtectedSubject):
		sreErrorCode(w, err.Error(), "protected_subject", http.StatusBadRequest)
	case errors.Is(err, facts.ErrInvalidType), errors.Is(err, facts.ErrInvalidKind),
		errors.Is(err, facts.ErrInvalidSubject), errors.Is(err, facts.ErrInvalidValue),
		errors.Is(err, facts.ErrInvalidSource), errors.Is(err, facts.ErrNoEvidence):
		sreErrorCode(w, err.Error(), "invalid_fact", http.StatusBadRequest)
	case errors.Is(err, facts.ErrNotFound):
		sreErrorCode(w, "fact not found", "not_found", http.StatusNotFound)
	case errors.Is(err, facts.ErrInvalidTransition):
		sreErrorCode(w, err.Error(), "invalid_transition", http.StatusConflict)
	case errors.Is(err, facts.ErrChanged):
		sreErrorCode(w, "the fact changed since it was shown", "content_changed",
			http.StatusConflict)
	default:
		internalError(w, r, "facts", err)
	}
}
