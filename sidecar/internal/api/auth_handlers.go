package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
)

func loginHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if req.Email == "" || req.Password == "" {
			jsonError(w, "email and password required",
				http.StatusBadRequest)
			return
		}

		user, err := auth.Authenticate(
			r.Context(), pool, req.Email, req.Password,
		)
		if err != nil {
			jsonError(w, "invalid credentials",
				http.StatusUnauthorized)
			return
		}

		sessionID, err := auth.CreateSession(
			r.Context(), pool, user.ID,
		)
		if err != nil {
			jsonError(w, "failed to create session",
				http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "sage_session",
			Value:    sessionID,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(auth.SessionDuration.Seconds()),
		})

		jsonResponse(w, map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		})
	}
}

func logoutHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("sage_session")
		if err == nil {
			_ = auth.DeleteSession(
				r.Context(), pool, cookie.Value,
			)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "sage_session",
			Value:    "",
			Path:     "/",
			HttpOnly: true,
			MaxAge:   -1,
		})
		jsonResponse(w, map[string]string{
			"status": "logged out",
		})
	}
}

func meHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "not authenticated",
				http.StatusUnauthorized)
			return
		}
		jsonResponse(w, map[string]any{
			"id":    user.ID,
			"email": user.Email,
			"role":  user.Role,
		})
	}
}

func listUsersHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := auth.ListUsers(r.Context(), pool)
		if err != nil {
			jsonError(w, "failed to list users",
				http.StatusInternalServerError)
			return
		}
		type userResp struct {
			ID        int        `json:"id"`
			Email     string     `json:"email"`
			Role      string     `json:"role"`
			CreatedAt time.Time  `json:"created_at"`
			LastLogin *time.Time `json:"last_login"`
		}
		resp := make([]userResp, len(users))
		for i, u := range users {
			resp[i] = userResp{
				ID:        u.ID,
				Email:     u.Email,
				Role:      u.Role,
				CreatedAt: u.CreatedAt,
				LastLogin: u.LastLogin,
			}
		}
		jsonResponse(w, map[string]any{"users": resp})
	}
}

func createUserHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if req.Email == "" || req.Password == "" {
			jsonError(w, "email and password required",
				http.StatusBadRequest)
			return
		}
		if req.Role == "" {
			req.Role = auth.RoleViewer
		}
		if !auth.IsValidRole(req.Role) {
			jsonError(w, "invalid role", http.StatusBadRequest)
			return
		}

		id, err := auth.CreateUser(
			r.Context(), pool,
			req.Email, req.Password, req.Role,
		)
		if err != nil {
			jsonError(w, "failed to create user: "+err.Error(),
				http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, map[string]any{
			"id":    id,
			"email": req.Email,
			"role":  req.Role,
		})
	}
}

func deleteUserHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			jsonError(w, "invalid user ID",
				http.StatusBadRequest)
			return
		}
		if err := auth.DeleteUser(
			r.Context(), pool, id,
		); err != nil {
			jsonError(w, err.Error(),
				http.StatusNotFound)
			return
		}
		jsonResponse(w, map[string]string{
			"status": "deleted",
		})
	}
}

func updateUserRoleHandler(
	pool *pgxpool.Pool,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := r.PathValue("id")
		id, err := strconv.Atoi(idStr)
		if err != nil {
			jsonError(w, "invalid user ID",
				http.StatusBadRequest)
			return
		}
		var req struct {
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if err := auth.UpdateUserRole(
			r.Context(), pool, id, req.Role,
		); err != nil {
			jsonError(w, err.Error(),
				http.StatusBadRequest)
			return
		}
		jsonResponse(w, map[string]string{
			"status": "updated",
		})
	}
}
