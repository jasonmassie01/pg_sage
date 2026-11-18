package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

// ux-09b: a channel's own quiet hours (the channel form), database and
// class mutes (the mute dialog), and the weekly line "N messages, M led to
// an action" on the Notifications page. Quiet hours and mutes are
// admin-only like channels; the weekly line is open to operators, like
// the delivery log.

// quietHoursSetter stores a channel's quiet hours.
type quietHoursSetter interface {
	SetChannelQuietHours(ctx context.Context, id int, start, end, tz string) error
}

// muteStore reads and writes mutes.
type muteStore interface {
	PutMute(ctx context.Context, m notify.Mute) (notify.Mute, error)
	DeleteMute(ctx context.Context, database, class string) (bool, error)
	ListMutes(ctx context.Context, now time.Time) ([]notify.Mute, error)
}

// weeklyLiner computes the weekly line.
type weeklyLiner interface {
	WeeklyLine(ctx context.Context, now time.Time) (notify.WeeklyLine, error)
}

func registerNotificationQuietMuteRoutes(
	mux *http.ServeMux, pool *pgxpool.Pool, ns *store.NotificationStore,
) {
	ps := notify.NewPoolStore(pool, nil)
	route(mux, "PUT /api/v1/notifications/channels/{id}/quiet-hours", gateAdmin,
		setChannelQuietHoursHandler(ns))
	route(mux, "GET /api/v1/notifications/mutes", gateAdmin, listMutesHandler(ps, time.Now))
	route(mux, "POST /api/v1/notifications/mutes", gateAdmin, createMuteHandler(ps, time.Now))
	route(mux, "DELETE /api/v1/notifications/mutes", gateAdmin, deleteMuteHandler(ps))
	route(mux, "GET /api/v1/notifications/weekly-line", gateOperator,
		weeklyLineHandler(ps, time.Now))
}

func setChannelQuietHoursHandler(qs quietHoursSetter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id <= 0 {
			jsonError(w, "invalid channel ID", http.StatusBadRequest)
			return
		}
		var req struct {
			Start    string `json:"start"`
			End      string `json:"end"`
			Timezone string `json:"timezone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if _, err := notify.ParseQuietHours(req.Start, req.End, req.Timezone); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		err = qs.SetChannelQuietHours(r.Context(), id, req.Start, req.End, req.Timezone)
		switch {
		case errors.Is(err, store.ErrNotFound):
			jsonError(w, "channel not found", http.StatusNotFound)
		case errors.Is(err, store.ErrInvalidQuietHours):
			jsonError(w, err.Error(), http.StatusBadRequest)
		case err != nil:
			slog.Error("set channel quiet hours", "channel", id, "error", err)
			jsonError(w, "quiet hours not saved", http.StatusInternalServerError)
		default:
			jsonResponse(w, map[string]string{"status": "updated"})
		}
	}
}

func listMutesHandler(ms muteStore, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mutes, err := ms.ListMutes(r.Context(), now())
		if err != nil {
			slog.Error("list notification mutes", "error", err)
			jsonError(w, "mutes unavailable", http.StatusInternalServerError)
			return
		}
		if mutes == nil {
			mutes = []notify.Mute{}
		}
		jsonResponse(w, map[string]any{"mutes": mutes})
	}
}

func createMuteHandler(ms muteStore, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		if user == nil {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var req struct {
			Database string `json:"database"`
			Class    string `json:"class"`
			Until    string `json:"until"`
			Reason   string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
		m := notify.Mute{Database: strings.TrimSpace(req.Database), Class: req.Class,
			Actor: muteActor(user.Email, user.ID), Reason: strings.TrimSpace(req.Reason)}
		if req.Until != "" {
			until, err := time.Parse(time.RFC3339, req.Until)
			if err != nil {
				jsonError(w, "until must be an RFC 3339 time, such as 2026-11-19T07:00:00Z",
					http.StatusBadRequest)
				return
			}
			m.Until = until
		}
		if err := notify.ValidateMute(m, now()); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		got, err := ms.PutMute(r.Context(), m)
		if err != nil {
			slog.Error("store notification mute", "database", m.Database, "error", err)
			jsonError(w, "mute not saved", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(got)
	}
}

// muteActor names the user who muted: the email, or the user id.
func muteActor(email string, id int) string {
	if email = strings.TrimSpace(email); email != "" {
		return email
	}
	return "user:" + strconv.Itoa(id)
}

func deleteMuteHandler(ms muteStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		db := strings.TrimSpace(r.URL.Query().Get("database"))
		class := r.URL.Query().Get("class")
		if db == "" {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if class != "" && !notify.Class(class).Valid() {
			jsonError(w, "unknown mute class "+strconv.Quote(class)+
				": use page, ask, digest or none", http.StatusBadRequest)
			return
		}
		removed, err := ms.DeleteMute(r.Context(), db, class)
		switch {
		case err != nil:
			slog.Error("delete notification mute", "database", db, "error", err)
			jsonError(w, "mute not removed", http.StatusInternalServerError)
		case !removed:
			jsonError(w, "no such mute", http.StatusNotFound)
		default:
			jsonResponse(w, map[string]string{"status": "unmuted"})
		}
	}
}

func weeklyLineHandler(wl weeklyLiner, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		line, err := wl.WeeklyLine(r.Context(), now())
		if err != nil {
			slog.Error("compute the weekly notification line", "error", err)
			jsonError(w, "weekly line unavailable", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, line)
	}
}
