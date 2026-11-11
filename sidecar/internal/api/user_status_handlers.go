package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

// pgForeignKeyViolation and pgUniqueViolation are SQLSTATEs.
const (
	pgForeignKeyViolation = "23503"
	pgUniqueViolation     = "23505"
)

// userIDParam reads the {id} path value; it reports false after a 400.
func userIDParam(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		jsonError(w, "invalid user ID", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

// setUserStatusHandler deactivates or re-enables a user (apiauth-01):
// PUT /api/v1/users/{id}/status {"disabled": bool}. Deactivation ends the
// user's sessions and revokes their operator MCP tokens; it is the default
// offboarding step, since the account's history stays attributable.
func setUserStatusHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := userIDParam(w, r)
		if !ok {
			return
		}
		var req struct {
			Disabled *bool `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if req.Disabled == nil {
			jsonError(w, "disabled (true or false) is required", http.StatusBadRequest)
			return
		}
		if caller := UserFromContext(r.Context()); *req.Disabled && caller != nil &&
			caller.ID == id {
			jsonError(w, "cannot deactivate your own account", http.StatusForbidden)
			return
		}
		res, err := auth.SetUserDisabled(r.Context(), pool, id, *req.Disabled)
		if err != nil {
			writeUserStatusError(w, r, err)
			return
		}
		status, event := "enabled", auth.AuditUserEnabled
		if *req.Disabled {
			status, event = "disabled", auth.AuditUserDisabled
		}
		if res.Changed {
			recordAuthEvent(r, pool, auth.AuthAuditEvent{
				Event: event, ActorUserID: actorID(r), TargetUserID: id,
				Detail: map[string]any{"sessions_ended": res.SessionsEnded,
					"mcp_tokens_revoked": res.TokensRevoked},
			})
		}
		jsonResponse(w, map[string]any{"status": status, "changed": res.Changed})
	}
}

func writeUserStatusError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		jsonError(w, "user not found", http.StatusNotFound)
	case errors.Is(err, auth.ErrLastAdmin):
		jsonError(w, "cannot deactivate the last enabled admin", http.StatusForbidden)
	default:
		internalError(w, r, "set user status", err)
	}
}

// writeDeleteUserError maps a refused delete. A foreign-key refusal is a
// 409 naming the referencing table, so the admin knows why and can
// deactivate the user instead.
func writeDeleteUserError(w http.ResponseWriter, r *http.Request, err error) {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, auth.ErrLastAdmin):
		jsonError(w, "cannot delete the last admin", http.StatusForbidden)
	case errors.Is(err, auth.ErrUserNotFound):
		jsonError(w, "user not found", http.StatusNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation:
		table := "another table"
		if pgErr.TableName != "" {
			table = pgErr.TableName
			if pgErr.SchemaName != "" {
				table = pgErr.SchemaName + "." + table
			}
		}
		jsonError(w, fmt.Sprintf(
			"user is still referenced by %s; deactivate the user instead", table),
			http.StatusConflict)
	default:
		internalError(w, r, "delete user", err)
	}
}
