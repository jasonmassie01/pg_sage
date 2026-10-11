package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/notify"
)

// breakGlassPath is the break-glass admin login (E1). It is outside the
// IdP on purpose; every use is audited and alerts every channel.
const breakGlassPath = "/api/v1/auth/break-glass"

// breakGlassAlerter delivers the security alert; *notify.Dispatcher.
type breakGlassAlerter interface {
	Broadcast(ctx context.Context, event notify.Event) (int, error)
}

func registerBreakGlassRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool, cfg *config.Config,
	alerter breakGlassAlerter,
) {
	mux.HandleFunc("POST "+breakGlassPath,
		breakGlassHandler(pool, cfg.OAuth.BreakGlass, alerter))
}

// breakGlassLimiterKey rate-limits break-glass attempts per client address,
// in the same limiter as password logins.
func breakGlassLimiterKey(ip string) string {
	return "break-glass|" + ip
}

func breakGlassHandler(
	pool *pgxpool.Pool, bg config.BreakGlassConfig, alerter breakGlassAlerter,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !bg.Enabled || bg.PasswordHash == "" {
			jsonError(w, "break-glass login is not enabled", http.StatusNotFound)
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		ip := requestClientIP(r)
		if !loginRateLimitDisabled && !loginLimiter.reserve(breakGlassLimiterKey(ip)) {
			breakGlassFailure(r, pool, "rate_limited")
			jsonError(w, "too many login attempts, try again later",
				http.StatusTooManyRequests)
			return
		}
		user, err := auth.BreakGlassLogin(r.Context(), pool, bg, req.Password)
		if err != nil {
			writeBreakGlassError(w, r, pool, err)
			return
		}
		loginLimiter.reset(breakGlassLimiterKey(ip))
		if !startSessionFor(w, r, pool, user.ID, auth.BreakGlassSessionDuration) {
			return
		}
		recordAuthEvent(r, pool, auth.AuthAuditEvent{
			Event: auth.AuditBreakGlassLogin, ActorUserID: user.ID, TargetUserID: user.ID,
			Detail: map[string]any{"method": "break_glass", "session_seconds": int(
				auth.BreakGlassSessionDuration.Seconds())},
		})
		alertBreakGlass(r.Context(), alerter, ip, user.ID)
		jsonResponse(w, map[string]any{
			"id": user.ID, "email": user.Email, "role": user.Role,
			"expires_in_seconds": int(auth.BreakGlassSessionDuration.Seconds()),
		})
	}
}

func writeBreakGlassError(
	w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool, err error,
) {
	switch {
	case errors.Is(err, auth.ErrBreakGlassDenied):
		breakGlassFailure(r, pool, "invalid_credentials")
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
	case errors.Is(err, auth.ErrBreakGlassConflict):
		breakGlassFailure(r, pool, "account_conflict")
		slog.Error("SECURITY: break-glass refused: its account email belongs to "+
			"another user", "email", auth.BreakGlassEmail)
		jsonError(w, "break-glass account unavailable", http.StatusConflict)
	default:
		breakGlassFailure(r, pool, "error")
		internalError(w, r, "break-glass login", err)
	}
}

func breakGlassFailure(r *http.Request, pool *pgxpool.Pool, reason string) {
	slog.Warn("SECURITY: break-glass login refused", "reason", reason,
		"source_ip", requestClientIP(r))
	recordAuthEvent(r, pool, auth.AuthAuditEvent{
		Event:  auth.AuditBreakGlassFailed,
		Detail: map[string]any{"method": "break_glass", "reason": reason},
	})
}

// alertBreakGlass logs the use prominently and broadcasts it to every
// enabled notification channel before the session is handed back.
func alertBreakGlass(ctx context.Context, alerter breakGlassAlerter, ip string, userID int) {
	now := time.Now().UTC()
	slog.Error("SECURITY: break-glass admin login used", "source_ip", ip,
		"user_id", userID, "at", now.Format(time.RFC3339))
	if alerter == nil {
		slog.Error("SECURITY: break-glass alert not sent: no notification dispatcher")
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	sent, err := alerter.Broadcast(actx, notify.Event{
		Type: notify.EventSecurityBreakGlass, Severity: "critical",
		Subject: "pg_sage break-glass admin login used",
		Body: fmt.Sprintf("The break-glass admin signed in from %s at %s. Review "+
			"what this session does and rotate the break-glass password.",
			ip, now.Format(time.RFC3339)),
		Data:     map[string]any{"source_ip": ip, "user_id": userID},
		DedupKey: "break-glass-" + now.Format(time.RFC3339Nano),
	})
	switch {
	case err != nil:
		slog.Error("SECURITY: break-glass alert failed", "error", err)
	case sent == 0:
		slog.Error("SECURITY: break-glass alert reached no notification channel; " +
			"configure one so break-glass use is noticed")
	}
}
