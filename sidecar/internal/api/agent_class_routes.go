package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/classify"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// Column classification (spec §6.7). Every signed-in role lists a
// database's classifications and asks how one column is classified;
// operators set a class (confirmed by the setting), confirm or reject a
// proposal, and run a rules scan, which only proposes.
//
//	GET  /api/v1/agent-classifications/{database}?status=&limit=&cursor=
//	GET  /api/v1/agent-classifications/{database}/column?schema=&table=&column=
//	PUT  /api/v1/agent-classifications/{database}/columns
//	POST /api/v1/agent-classifications/{database}/{id}/confirm|reject
//	POST /api/v1/agent-classifications/{database}/scan

const maxClassBody = 4 << 10

type classHandlers struct{ mgr *fleet.DatabaseManager }

func registerAgentClassRoutes(mux *http.ServeMux, mgr *fleet.DatabaseManager) {
	viewer := RequireRole("admin", "operator", "viewer")
	operatorUp := RequireRole("admin", "operator")
	h := classHandlers{mgr: mgr}
	const base = "/api/v1/agent-classifications/{database}"
	mux.Handle("GET "+base, viewer(http.HandlerFunc(h.list)))
	mux.Handle("GET "+base+"/column", viewer(http.HandlerFunc(h.column)))
	mux.Handle("PUT "+base+"/columns", operatorUp(http.HandlerFunc(h.set)))
	mux.Handle("POST "+base+"/scan", operatorUp(http.HandlerFunc(h.scan)))
	for _, verb := range []string{"confirm", "reject"} {
		mux.Handle("POST "+base+"/{id}/"+verb, operatorUp(h.decide(verb == "confirm")))
	}
}

// store resolves {database} to its classification store.
func (h classHandlers) store(w http.ResponseWriter, r *http.Request) (*classify.Store,
	bool) {
	name := r.PathValue("database")
	if name == "" || validateDatabaseParam(name) != nil {
		sreErrorCode(w, "invalid database name", "invalid_arguments",
			http.StatusUnprocessableEntity)
		return nil, false
	}
	var inst *fleet.DatabaseInstance
	if h.mgr != nil {
		inst = h.mgr.GetInstance(name)
	}
	if inst == nil || inst.Pool == nil {
		sreErrorCode(w, "unknown database", "unknown_database", http.StatusNotFound)
		return nil, false
	}
	return classify.NewStore(inst.Pool), true
}

// classView is one classification as served.
type classView struct {
	ID           int64               `json:"id"`
	RelID        uint32              `json:"relid"`
	AttNum       int16               `json:"attnum"`
	Schema       string              `json:"schema"`
	Table        string              `json:"table"`
	Column       string              `json:"column"`
	Type         string              `json:"type"`
	Class        classify.Class      `json:"class"`
	Status       classify.Status     `json:"status"`
	Effective    classify.Class      `json:"effective"`
	Source       classify.Source     `json:"source"`
	ProposedBy   string              `json:"proposed_by"`
	Rationale    string              `json:"rationale"`
	Evidence     []classify.Citation `json:"evidence"`
	DecidedBy    string              `json:"decided_by,omitempty"`
	DecidedAt    *time.Time          `json:"decided_at,omitempty"`
	DecisionNote string              `json:"decision_note,omitempty"`
	Proposals    int                 `json:"proposals"`
	Live         bool                `json:"live"`
	Hash         string              `json:"hash"`
	UpdatedAt    time.Time           `json:"updated_at"`
}

func classViewOf(c classify.Classification) classView {
	return classView{ID: c.ID, RelID: c.Column.RelID, AttNum: c.Column.AttNum,
		Schema: c.Column.Schema, Table: c.Column.Table, Column: c.Column.Name,
		Type: c.Column.Type, Class: c.Class, Status: c.Status, Effective: c.Effective(),
		Source: c.Source, ProposedBy: c.ProposedBy, Rationale: c.Rationale,
		Evidence: c.Evidence, DecidedBy: c.DecidedBy, DecidedAt: c.DecidedAt,
		DecisionNote: c.DecisionNote, Proposals: c.Proposals, Live: c.Live, Hash: c.Hash(),
		UpdatedAt: c.UpdatedAt}
}

func (h classHandlers) list(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := classify.Filter{}
	for _, st := range strings.Split(q.Get("status"), ",") {
		if st = strings.TrimSpace(st); st != "" {
			f.Status = append(f.Status, classify.Status(st))
		}
	}
	var err error
	if f.Limit, err = optionalInt(q.Get("limit")); err != nil || f.Limit < 0 {
		sreErrorCode(w, "limit must be 1-200", "invalid_arguments",
			http.StatusUnprocessableEntity)
		return
	}
	if v := q.Get("cursor"); v != "" {
		if f.After, err = strconv.ParseInt(v, 10, 64); err != nil || f.After < 0 {
			sreErrorCode(w, "invalid cursor", "invalid_arguments",
				http.StatusUnprocessableEntity)
			return
		}
	}
	got, next, err := s.List(r.Context(), f)
	if err != nil {
		writeClassError(w, r, err)
		return
	}
	items := make([]classView, 0, len(got))
	for _, c := range got {
		items = append(items, classViewOf(c))
	}
	cursor := ""
	if next > 0 {
		cursor = strconv.FormatInt(next, 10)
	}
	jsonResponse(w, map[string]any{"items": items, "next_cursor": cursor})
}

func optionalInt(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.Atoi(s)
}

func (h classHandlers) column(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	schema, table, column := q.Get("schema"), q.Get("table"), q.Get("column")
	if schema == "" || table == "" || column == "" {
		sreErrorCode(w, "schema, table and column are required", "invalid_arguments",
			http.StatusUnprocessableEntity)
		return
	}
	col, err := s.ResolveColumn(r.Context(), schema, table, column)
	if err != nil {
		writeClassError(w, r, err)
		return
	}
	eff, err := s.ClassOf(r.Context(), col.RelID, col.AttNum)
	if err != nil {
		writeClassError(w, r, err)
		return
	}
	jsonResponse(w, map[string]any{"database": r.PathValue("database"), "schema": col.Schema,
		"table": col.Table, "column": col.Name, "relid": col.RelID, "attnum": col.AttNum,
		"type": col.Type, "class": eff.Class, "confirmed": eff.Confirmed,
		"classified": eff.Class != classify.Unclassified, "fact_id": eff.FactID})
}

// decodeOptional reads a JSON body into out; an empty body leaves it zero.
func decodeOptional(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxClassBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		sreErrorCode(w, "invalid body", "invalid_arguments", http.StatusUnprocessableEntity)
		return false
	}
	return true
}

func (h classHandlers) set(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	var body struct {
		Schema string `json:"schema"`
		Table  string `json:"table"`
		Column string `json:"column"`
		Class  string `json:"class"`
		Note   string `json:"note"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	class, err := classify.ParseClass(body.Class)
	if err != nil || body.Schema == "" || body.Table == "" {
		sreErrorCode(w, "schema, table and a class (clean, pii, secret, untrusted_input) "+
			"are required", "invalid_arguments", http.StatusUnprocessableEntity)
		return
	}
	var col classify.Column
	if body.Column == "" {
		col, err = s.ResolveTable(r.Context(), body.Schema, body.Table)
	} else {
		col, err = s.ResolveColumn(r.Context(), body.Schema, body.Table, body.Column)
	}
	if err != nil {
		writeClassError(w, r, err)
		return
	}
	c, err := s.Set(r.Context(), col, class, factActor(r), body.Note)
	if err != nil {
		writeClassError(w, r, err)
		return
	}
	jsonResponse(w, classViewOf(c))
}

func (h classHandlers) decide(confirm bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.store(w, r)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			sreErrorCode(w, "invalid id", "invalid_arguments", http.StatusUnprocessableEntity)
			return
		}
		var body struct {
			Note       string `json:"note"`
			ExpectHash string `json:"expect_hash"`
		}
		if !decodeOptional(w, r, &body) {
			return
		}
		c, err := s.Decide(r.Context(), id, confirm, factActor(r), body.Note, body.ExpectHash)
		if err != nil {
			writeClassError(w, r, err)
			return
		}
		jsonResponse(w, classViewOf(c))
	}
}

func (h classHandlers) scan(w http.ResponseWriter, r *http.Request) {
	s, ok := h.store(w, r)
	if !ok {
		return
	}
	var body struct {
		Cursor string `json:"cursor"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	after, err := parseClassCursor(body.Cursor)
	if err != nil {
		sreErrorCode(w, "invalid cursor", "invalid_arguments", http.StatusUnprocessableEntity)
		return
	}
	res, err := classify.Scan(r.Context(), s, classify.ScanOptions{After: after})
	if err != nil {
		writeClassError(w, r, err)
		return
	}
	next := ""
	if !res.Done {
		next = fmt.Sprintf("%d.%d", res.Next.RelID, res.Next.AttNum)
	}
	jsonResponse(w, map[string]any{"read": res.Read, "proposed": res.Proposed,
		"done": res.Done, "next_cursor": next})
}

// parseClassCursor reads "relid.attnum" ("" is the start).
func parseClassCursor(s string) (classify.Cursor, error) {
	if s == "" {
		return classify.Cursor{}, nil
	}
	rel, att, ok := strings.Cut(s, ".")
	relid, err1 := strconv.ParseUint(rel, 10, 32)
	attnum, err2 := strconv.ParseInt(att, 10, 16)
	if !ok || err1 != nil || err2 != nil || attnum < 0 {
		return classify.Cursor{}, fmt.Errorf("cursor %q is not relid.attnum", s)
	}
	return classify.Cursor{RelID: uint32(relid), AttNum: int16(attnum)}, nil
}

func writeClassError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, classify.ErrColumnNotFound), errors.Is(err, classify.ErrNotFound):
		sreErrorCode(w, err.Error(), "not_found", http.StatusNotFound)
	case errors.Is(err, classify.ErrInvalidClass), errors.Is(err, classify.ErrInvalidActor),
		errors.Is(err, classify.ErrInvalidStatus):
		sreErrorCode(w, err.Error(), "invalid_arguments", http.StatusUnprocessableEntity)
	case errors.Is(err, classify.ErrInvalidTransition):
		sreErrorCode(w, err.Error(), "invalid_transition", http.StatusConflict)
	case errors.Is(err, classify.ErrChanged):
		sreErrorCode(w, err.Error(), "content_changed", http.StatusConflict)
	default:
		internalError(w, r, "agent classification", err)
	}
}
