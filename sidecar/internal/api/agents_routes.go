package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/upkeep"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// Agent principals (agent governance, AGENTDB-SPEC §8.3): operators list
// and read them; admins create them with an accountable sponsor, change
// them, retire them and mint their MCP tokens. A widening change (a higher
// ceiling, another profile) needs a second person unless
// agents.single_operator_mode is on, when one admin applies it with a
// recorded reason.

type agentRoutes struct {
	pool           *pgxpool.Pool
	store          *agentguard.Store
	tokens         *mcptoken.Store
	singleOperator bool
}

func registerAgentRoutes(mux *http.ServeMux, pool *pgxpool.Pool, cfg *config.Config) {
	r := &agentRoutes{pool: pool, store: agentguard.NewStore(pool),
		tokens:         mcptoken.NewStore(pool),
		singleOperator: cfg != nil && cfg.Agents.SingleOperatorMode}
	operator := RequireRole("admin", "operator")
	admin := RequireRole("admin")
	mux.Handle("GET /api/v1/agents", operator(http.HandlerFunc(r.list)))
	mux.Handle("POST /api/v1/agents", admin(http.HandlerFunc(r.create)))
	mux.Handle("GET /api/v1/agents/{id}", operator(http.HandlerFunc(r.get)))
	mux.Handle("PATCH /api/v1/agents/{id}", admin(http.HandlerFunc(r.patch)))
	mux.Handle("POST /api/v1/agents/{id}/tokens", admin(http.HandlerFunc(r.mintToken)))
}

// decodeStrict reads a JSON body, refusing unknown fields.
func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		jsonError(w, "invalid request body", http.StatusUnprocessableEntity)
		return false
	}
	return true
}

func (a *agentRoutes) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			jsonError(w, "limit must be a number from 1 to 200", http.StatusUnprocessableEntity)
			return
		}
		limit = n
	}
	page, err := a.store.List(r.Context(), agentguard.ListOptions{Limit: limit,
		Cursor: q.Get("cursor"), Status: agentguard.Status(q.Get("status"))})
	if err != nil {
		agentError(w, r, "list agents", err)
		return
	}
	jsonResponse(w, page)
}

type createAgentBody struct {
	Name          string `json:"name"`
	SponsorUserID int    `json:"sponsor_user_id"`
	Profile       string `json:"profile"`
	EnvCeiling    string `json:"env_ceiling"`
	Tenant        string `json:"tenant"`
}

func (a *agentRoutes) create(w http.ResponseWriter, r *http.Request) {
	var body createAgentBody
	if !decodeStrict(w, r, &body) {
		return
	}
	if body.SponsorUserID <= 0 {
		jsonError(w, "sponsor_user_id is required: every agent has an accountable sponsor",
			http.StatusUnprocessableEntity)
		return
	}
	p, err := a.store.Create(r.Context(), agentguard.CreateRequest{Name: body.Name,
		SponsorUserID: &body.SponsorUserID, Profile: body.Profile,
		EnvCeiling: agentguard.Env(body.EnvCeiling), Tenant: body.Tenant,
		CreatedBy: authenticatedActor(r)})
	if err != nil {
		agentError(w, r, "create agent", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(p)
}

func (a *agentRoutes) get(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		agentError(w, r, "get agent", err)
		return
	}
	jsonResponse(w, p)
}

type patchAgentBody struct {
	SponsorUserID *int    `json:"sponsor_user_id"`
	Profile       *string `json:"profile"`
	EnvCeiling    *string `json:"env_ceiling"`
	Status        *string `json:"status"`
	Reason        string  `json:"reason"`
}

func (b patchAgentBody) patch() agentguard.Patch {
	p := agentguard.Patch{SponsorUserID: b.SponsorUserID, Profile: b.Profile}
	if b.EnvCeiling != nil {
		env := agentguard.Env(*b.EnvCeiling)
		p.EnvCeiling = &env
	}
	return p
}

func (a *agentRoutes) patch(w http.ResponseWriter, r *http.Request) {
	var body patchAgentBody
	if !decodeStrict(w, r, &body) {
		return
	}
	if body.Status != nil && *body.Status != string(agentguard.StatusRetired) {
		jsonError(w, `status may only be set to "retired"; freeze and unfreeze have `+
			"their own endpoints", http.StatusUnprocessableEntity)
		return
	}
	cur, err := a.store.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		agentError(w, r, "update agent", err)
		return
	}
	patch := body.patch()
	if patch.Widens(cur) && !a.allowWidening(w, r, cur, body.Reason) {
		return
	}
	p, err := a.store.Update(r.Context(), cur.ID, patch)
	if err == nil && body.Status != nil {
		// The leader drops the roles after the grace under this admin's approval.
		err = upkeep.RecordRetiringAdmin(r.Context(), a.pool, cur.ID, sessionUserID(r))
	}
	if err == nil && body.Status != nil {
		p, err = a.store.SetStatus(r.Context(), cur.ID, agentguard.StatusRetired, body.Reason)
	}
	if err != nil {
		agentError(w, r, "update agent", err)
		return
	}
	jsonResponse(w, p)
}

// allowWidening applies the two-person rule (§6.11): a widening change
// needs a second admin. In single-operator mode one admin may apply it
// with a reason, which is logged for the post-hoc review.
func (a *agentRoutes) allowWidening(w http.ResponseWriter, r *http.Request,
	cur agentguard.Principal, reason string) bool {
	if !a.singleOperator {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "two_person_required",
			"error": "a higher ceiling or another profile widens what the agent may do " +
				"and needs a second admin (or agents.single_operator_mode with a reason)"})
		return false
	}
	if strings.TrimSpace(reason) == "" {
		jsonError(w, "a widening change in single-operator mode needs a reason",
			http.StatusUnprocessableEntity)
		return false
	}
	slog.Warn("agent widened by a single operator; review it", "agent", cur.Name,
		"principal_id", cur.ID, "actor", authenticatedActor(r), "reason", reason)
	return true
}

type mintTokenBody struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	Databases     []string `json:"databases"`
	ExpiresInDays int      `json:"expires_in_days"`
}

func (a *agentRoutes) mintToken(w http.ResponseWriter, r *http.Request) {
	var body mintTokenBody
	if !decodeStrict(w, r, &body) {
		return
	}
	if body.ExpiresInDays < 1 || body.ExpiresInDays > maxMCPTokenDays {
		jsonError(w, "expires_in_days must be between 1 and 90",
			http.StatusUnprocessableEntity)
		return
	}
	tok, err := agentguard.IssueToken(r.Context(), a.store, a.tokens, r.PathValue("id"),
		agentguard.TokenRequest{Name: body.Name, Scopes: body.Scopes,
			Databases: body.Databases, CreatedBy: authenticatedActor(r),
			ExpiresIn: time.Duration(body.ExpiresInDays) * 24 * time.Hour})
	if err != nil {
		agentError(w, r, "mint agent token", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(tok)
}

// agentError maps agentguard errors to the §8.1 statuses; storage errors
// are logged and hidden.
func agentError(w http.ResponseWriter, r *http.Request, op string, err error) {
	msg := strings.TrimPrefix(err.Error(), "agentguard: ")
	if d, ok := agentguard.IsDenied(err); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"verdict": "blocked",
			"reason_code": string(d.Reason), "error": d.Detail, "fix": d.Fix})
		return
	}
	switch {
	case errors.Is(err, agentguard.ErrNotFound):
		jsonError(w, "agent not found", http.StatusNotFound)
	case errors.Is(err, agentguard.ErrInvalid), errors.Is(err, agentguard.ErrSponsorNotFound):
		jsonError(w, msg, http.StatusUnprocessableEntity)
	case errors.Is(err, agentguard.ErrDuplicateName), errors.Is(err, agentguard.ErrRetired):
		jsonError(w, msg, http.StatusConflict)
	case errors.Is(err, agentguard.ErrUnavailable):
		jsonError(w, "agent store unavailable", http.StatusServiceUnavailable)
	default:
		internalError(w, r, op, err)
	}
}
