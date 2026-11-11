package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/upkeep"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// Agent principals (agent governance, spec §8.3): operators list
// and read them; admins create them with an accountable sponsor, change
// them, retire them and mint their MCP tokens. A widening change (a higher
// ceiling, a broader profile) waits as a pending change (202) for a second
// admin's confirm (agent_change_routes.go, agentgov-06) unless
// agents.single_operator_mode is on, when one admin applies it with a
// recorded reason. Every change writes one sage.auth_audit event in the
// change's own transaction (agentgov-05); a failed audit write fails it.

type agentRoutes struct {
	pool           *pgxpool.Pool
	store          *agentguard.Store
	tokens         *mcptoken.Store
	singleOperator bool
	// retireGraceDays is agents.roles.retire_grace_days, which the retire
	// dialog shows (agentgov-07).
	retireGraceDays int
	// changeTTL is how long a widening change waits for the second admin:
	// the unfreeze approval TTL (15 minutes).
	changeTTL time.Duration
}

func registerAgentRoutes(mux *http.ServeMux, pool *pgxpool.Pool, cfg *config.Config) {
	r := &agentRoutes{pool: pool, store: agentguard.NewStore(pool),
		tokens:          mcptoken.NewStore(pool),
		singleOperator:  cfg != nil && cfg.Agents.SingleOperatorMode,
		changeTTL:       agentguard.DefaultKillConfig().ApprovalTTL,
		retireGraceDays: config.DefaultAgentRetireGraceDays}
	if cfg != nil {
		r.retireGraceDays = cfg.Agents.Roles.RetireGraceDays
	}
	route(mux, "GET /api/v1/agents", gateOperator, http.HandlerFunc(r.list))
	route(mux, "POST /api/v1/agents", gateAdmin, http.HandlerFunc(r.create))
	route(mux, "GET /api/v1/agents/{id}", gateOperator, http.HandlerFunc(r.get))
	route(mux, "PATCH /api/v1/agents/{id}", gateAdmin, http.HandlerFunc(r.patch))
	route(mux, "POST /api/v1/agents/{id}/tokens", gateAdmin, http.HandlerFunc(r.mintToken))
	route(mux, "GET /api/v1/agents/{id}/changes", gateOperator, http.HandlerFunc(r.changes))
	route(mux, "POST /api/v1/agents/{id}/changes/{cid}/confirm", gateAdmin,
		http.HandlerFunc(r.confirmChange))
}

// governanceAudit is the audit of a change the signed-in admin makes.
func governanceAudit(r *http.Request, reason string) agentguard.Audit {
	return agentguard.Audit{ActorUserID: sessionUserID(r), SourceIP: requestClientIP(r),
		Reason: reason}
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
	jsonResponse(w, agentListPage{Page: page, Profiles: agentguard.ProfileCatalog(),
		RetireGraceDays: a.retireGraceDays})
}

// agentListPage is a page of principals with the profiles pg_sage ships,
// so a client offers only those (agentgov-06), and the days a retired
// agent's roles are kept (agentgov-07).
type agentListPage struct {
	agentguard.Page
	Profiles        []agentguard.ProfileInfo `json:"profiles"`
	RetireGraceDays int                      `json:"retire_grace_days"`
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
		CreatedBy: authenticatedActor(r), Audit: governanceAudit(r, "")})
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
	patch, audit := body.patch(), governanceAudit(r, body.Reason)
	// cur is an unlocked read, so the store judges a narrowing again under
	// the row lock (agentgov-06).
	update := a.store.UpdateNarrowing
	if patch.Widens(cur) {
		if !a.singleOperator {
			a.requestChange(w, r, cur, patch, body)
			return
		}
		if !allowSingleOperator(w, body.Reason) {
			return
		}
		audit.Event, update = auth.AuditAgentWidenedSingleOperator, a.store.Update
	}
	p, err := update(r.Context(), cur.ID, patch, audit)
	if err == nil && body.Status != nil {
		// The leader drops the roles after the grace under this admin's approval.
		err = upkeep.RecordRetiringAdmin(r.Context(), a.pool, cur.ID, sessionUserID(r))
	}
	if err == nil && body.Status != nil {
		p, err = a.store.SetStatus(r.Context(), cur.ID, agentguard.StatusRetired,
			governanceAudit(r, body.Reason))
	}
	if err != nil {
		agentError(w, r, "update agent", err)
		return
	}
	jsonResponse(w, p)
}

// allowSingleOperator applies the two-person rule's single-operator form
// (§6.11): one admin may apply a widening change with a reason; it is
// recorded as agent_widened_single_operator for the post-hoc review.
func allowSingleOperator(w http.ResponseWriter, reason string) bool {
	if strings.TrimSpace(reason) == "" {
		jsonError(w, "a widening change in single-operator mode needs a reason",
			http.StatusUnprocessableEntity)
		return false
	}
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
			ExpiresIn: time.Duration(body.ExpiresInDays) * 24 * time.Hour,
			Audit:     governanceAudit(r, "")})
	if err != nil {
		agentError(w, r, "mint agent token", err)
		return
	}
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
	case changeError(w, err, msg):
	case errors.Is(err, agentguard.ErrNotFound):
		jsonError(w, "agent not found", http.StatusNotFound)
	case errors.Is(err, agentguard.ErrSponsorChangeFrozen):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"code": "sponsor_change_frozen",
			"error": msg})
	case errors.Is(err, agentguard.ErrInvalid), errors.Is(err, agentguard.ErrSponsorNotFound),
		errors.Is(err, agentguard.ErrSponsorNotEligible):
		jsonError(w, msg, http.StatusUnprocessableEntity)
	case errors.Is(err, agentguard.ErrDuplicateName), errors.Is(err, agentguard.ErrRetired):
		jsonError(w, msg, http.StatusConflict)
	case errors.Is(err, agentguard.ErrUnavailable):
		jsonError(w, "agent store unavailable", http.StatusServiceUnavailable)
	default:
		internalError(w, r, op, err)
	}
}
