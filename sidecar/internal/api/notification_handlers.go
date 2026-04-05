package api

import (
	"encoding/json"
	"net/http"
	"strconv"

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
			channels = []notify.Channel{}
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
			jsonError(w, err.Error(), http.StatusBadRequest)
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
			Enabled bool              `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}

		if err := ns.UpdateChannel(
			r.Context(), id, req.Name,
			req.Config, req.Enabled,
		); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
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
			jsonError(w, err.Error(),
				http.StatusInternalServerError)
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
			jsonError(w, "test failed: "+err.Error(),
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
		if req.MinSeverity == "" {
			req.MinSeverity = "warning"
		}

		id, err := ns.CreateRule(
			r.Context(), req.ChannelID,
			req.Event, req.MinSeverity)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
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
			jsonError(w, err.Error(),
				http.StatusInternalServerError)
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
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid request body",
				http.StatusBadRequest)
			return
		}
		if err := ns.UpdateRule(
			r.Context(), id, req.Enabled,
		); err != nil {
			jsonError(w, err.Error(),
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]string{"status": "updated"})
	}
}

func listNotificationLogHandler(
	ns *store.NotificationStore,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := parseIntDefault(
			r.URL.Query().Get("limit"), 100)
		entries, err := ns.ListLog(r.Context(), limit)
		if err != nil {
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

// maskChannelSecrets redacts sensitive values in a channel config
// map so they are not returned verbatim via the API. Values longer
// than 8 characters are partially shown (first 4 + last 4); shorter
// values are fully masked.
func maskChannelSecrets(config map[string]string) {
	sensitiveKeys := []string{
		"webhook_url", "routing_key", "smtp_password",
		"api_key", "token", "secret",
	}
	for _, key := range sensitiveKeys {
		if v, ok := config[key]; ok && len(v) > 8 {
			config[key] = v[:4] + "****" + v[len(v)-4:]
		} else if ok {
			config[key] = "****"
		}
	}
}
