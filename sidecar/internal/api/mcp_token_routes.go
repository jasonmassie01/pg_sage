package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// MCP API tokens (admin only). A token's secret is shown once, in the
// create response; listings carry only its prefix.

const maxMCPTokenDays = int(mcptoken.MaxLifetime / (24 * time.Hour))

type createMCPTokenBody struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Scopes        []string `json:"scopes"`
	Databases     []string `json:"databases"`
	ExpiresInDays int      `json:"expires_in_days"`
	OwnerUserID   int      `json:"owner_user_id"`
}

func registerMCPTokenRoutes(mux *http.ServeMux, pool *pgxpool.Pool) {
	store := mcptoken.NewStore(pool)
	adminOnly := RequireRole("admin")
	mux.Handle("GET /api/v1/mcp/tokens", adminOnly(listMCPTokensHandler(store)))
	mux.Handle("POST /api/v1/mcp/tokens", adminOnly(createMCPTokenHandler(store)))
	mux.Handle("DELETE /api/v1/mcp/tokens/{id}", adminOnly(revokeMCPTokenHandler(store)))
}

func listMCPTokensHandler(store *mcptoken.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tokens, err := store.List(r.Context())
		if err != nil {
			internalError(w, r, "list mcp tokens", err)
			return
		}
		jsonResponse(w, map[string]any{"tokens": tokens})
	}
}

func createMCPTokenHandler(store *mcptoken.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body createMCPTokenBody
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if body.ExpiresInDays < 1 || body.ExpiresInDays > maxMCPTokenDays {
			jsonError(w, "expires_in_days must be between 1 and 90",
				http.StatusBadRequest)
			return
		}
		tok, err := store.Create(r.Context(), mcptoken.CreateRequest{
			Name: body.Name, Kind: mcptoken.Kind(body.Kind), Scopes: body.Scopes,
			Databases:   body.Databases,
			ExpiresIn:   time.Duration(body.ExpiresInDays) * 24 * time.Hour,
			OwnerUserID: body.OwnerUserID, CreatedBy: authenticatedActor(r),
		})
		if err != nil {
			mcpTokenError(w, r, "create mcp token", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(tok)
	}
}

func revokeMCPTokenHandler(store *mcptoken.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, err := store.Revoke(r.Context(), r.PathValue("id"), authenticatedActor(r))
		if err != nil {
			mcpTokenError(w, r, "revoke mcp token", err)
			return
		}
		jsonResponse(w, tok)
	}
}

// mcpTokenError maps store errors: refusals are the caller's to fix (400),
// an unknown token is 404, anything else is logged and hidden.
func mcpTokenError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, mcptoken.ErrNotFound):
		jsonError(w, "token not found", http.StatusNotFound)
	case errors.Is(err, mcptoken.ErrInvalid), errors.Is(err, mcptoken.ErrApproveForAgent),
		errors.Is(err, mcptoken.ErrOwnerRequired):
		jsonError(w, strings.TrimPrefix(err.Error(), "mcptoken: "), http.StatusBadRequest)
	default:
		internalError(w, r, op, err)
	}
}
