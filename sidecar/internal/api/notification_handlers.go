package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/notify"
	"github.com/pg-sage/sidecar/internal/store"
)

func listChannelsHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		channels, err := ns.ListChannels(r.Context())
		if err != nil {
			jsonError(w, "failed to list channels",
				http.StatusInternalServerError)
			return
		}
		if channels == nil {
			channels = []store.ChannelListing{}
		}
		for i := range channels {
			maskChannelSecrets(channels[i].Config)
		}
		jsonResponse(w, map[string]any{
			"channels": channels,
		})
	}
}

func createChannelHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name   string            `json:"name"`
			Type   string            `json:"type"`
			Config map[string]string `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.Type == "" {
			jsonError(w, "name and type required",
				http.StatusBadRequest)
			return
		}

		user := UserFromContext(r.Context())
		userID := 0
		if user != nil {
			userID = user.ID
		}

		id, err := ns.CreateChannel(
			r.Context(), req.Name, req.Type,
			req.Config, userID)
		if err != nil {
			if errors.Is(err, store.ErrValidation) {
				jsonError(w, err.Error(),
					http.StatusBadRequest)
				return
			}
			internalError(w, r, "create channel", err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, map[string]any{
			"id":   id,
			"name": req.Name,
			"type": req.Type,
		})
	}
}

func updateChannelHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid channel ID",
				http.StatusBadRequest)
			return
		}

		var req struct {
			Name    string            `json:"name"`
			Config  map[string]string `json:"config"`
			Enabled *bool             `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}

		existing, err := ns.GetChannel(r.Context(), id)
		if err != nil {
			notificationWriteError(w, r, "get channel", "channel not found", err)
			return
		}
		if req.Name == "" {
			req.Name = existing.Name
		}
		if req.Config == nil {
			req.Config = existing.Config
		}
		enabled := existing.Enabled
		if req.Enabled != nil {
			enabled = *req.Enabled
		}

		if err := ns.UpdateChannel(
			r.Context(), id, req.Name,
			req.Config, enabled,
		); err != nil {
			notificationWriteError(w, r, "update channel", "channel not found", err)
			return
		}
		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

func deleteChannelHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid channel ID",
				http.StatusBadRequest)
			return
		}
		if err := ns.DeleteChannel(r.Context(), id); err != nil {
			notificationWriteError(w, r, "delete channel", "channel not found", err)
			return
		}
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

func testChannelHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid channel ID",
				http.StatusBadRequest)
			return
		}
		if err := ns.TestChannel(r.Context(), id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				jsonError(w, "channel not found", http.StatusNotFound)
				return
			}
			if errors.Is(err, store.ErrValidation) {
				jsonError(w, err.Error(), http.StatusBadRequest)
				return
			}
			// Never echo raw delivery errors: they can carry webhook
			// secrets and act as an SSRF/port-scan oracle (G7-B10/B21).
			jsonError(w, store.ErrTestDeliveryFailed.Error(),
				http.StatusBadGateway)
			return
		}
		jsonResponse(w, map[string]string{
			"status": "test sent",
		})
	}
}

func listRulesHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rules, err := ns.ListRules(r.Context())
		if err != nil {
			jsonError(w, "failed to list rules",
				http.StatusInternalServerError)
			return
		}
		if rules == nil {
			rules = []notify.Rule{}
		}
		jsonResponse(w, map[string]any{"rules": rules})
	}
}

func createRuleHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChannelID   int    `json:"channel_id"`
			Event       string `json:"event"`
			MinSeverity string `json:"min_severity"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		req.MinSeverity = defaultRuleSeverity(req.Event, req.MinSeverity)

		id, err := ns.CreateRule(
			r.Context(), req.ChannelID,
			req.Event, req.MinSeverity)
		if err != nil {
			notificationWriteError(w, r, "create rule", "channel not found", err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		jsonResponse(w, map[string]any{
			"id":           id,
			"channel_id":   req.ChannelID,
			"event":        req.Event,
			"min_severity": req.MinSeverity,
		})
	}
}

func deleteRuleHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid rule ID",
				http.StatusBadRequest)
			return
		}
		if err := ns.DeleteRule(r.Context(), id); err != nil {
			notificationWriteError(w, r, "delete rule", "rule not found", err)
			return
		}
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

func updateRuleHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonError(w, "invalid rule ID",
				http.StatusBadRequest)
			return
		}
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		// A missing field used to decode as false and silently disable
		// the rule (G7-B29).
		if req.Enabled == nil {
			jsonError(w, "enabled (boolean) is required",
				http.StatusBadRequest)
			return
		}
		if err := ns.UpdateRule(
			r.Context(), id, *req.Enabled,
		); err != nil {
			notificationWriteError(w, r, "update rule", "rule not found", err)
			return
		}
		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

// listNotificationLogHandler serves the one notification log
// (alerting-06c), filtered by ?database= like the other list endpoints:
// a malformed name is 400, one pg_sage does not monitor is 404, and "all"
// or none lists every database.
func listNotificationLogHandler(
	ns *store.NotificationStore, mgr *fleet.DatabaseManager,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		database, ok := readDatabaseParam(w, r)
		if !ok || rejectUnknownDatabase(w, mgr, database) {
			return
		}
		if database == "all" {
			database = ""
		}
		limit := parseIntDefault(
			r.URL.Query().Get("limit"), 100)
		entries, err := ns.ListLogFor(r.Context(), database, limit)
		if err != nil {
			slog.Error("list notification log failed", "database", database, "error", err)
			jsonError(w, "failed to list log",
				http.StatusInternalServerError)
			return
		}
		if entries == nil {
			entries = []store.NotificationLogEntry{}
		}
		jsonResponse(w, map[string]any{"log": entries})
	}
}

// defaultRuleSeverity returns the requested min_severity, or the event's
// own fixed severity when none is given, so a default rule can always
// fire (G7-B06: the old "warning" default never matched info events).
func defaultRuleSeverity(event, requested string) string {
	if requested != "" {
		return requested
	}
	return notify.DefaultMinSeverity(event)
}

// maskChannelSecrets redacts sensitive values in a channel config
// map so they are not returned verbatim via the API. Values longer
// than 8 characters are partially shown (first 4 + last 4); shorter
// values are fully masked.
func maskChannelSecrets(config map[string]string) {
	// Every key the store seals, so a new channel type's credential is
	// masked too (market-14: webhook headers were returned in full).
	sensitiveKeys := notify.SecretConfigKeys
	for _, key := range sensitiveKeys {
		if v, ok := config[key]; ok && len(v) > 8 {
			config[key] = v[:4] + "****" + v[len(v)-4:]
		} else if ok {
			config[key] = "****"
		}
	}
}
