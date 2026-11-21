package api

import (
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/store"
)

// GET /api/v1/config/yaml (cli-01b) renders process-wide settings as the
// YAML a config file would hold to set them, for `pg_sage_sidecar config
// export` and the Settings page's "equivalent YAML" after a save. It
// reads the desired config (stored overrides replayed) and writes
// nothing. ?keys=a,b names the settings (keys of GET /api/v1/config/global);
// ?overrides=1 renders only the keys a stored global override sets; with
// neither it renders every setting. A secret is always its ${NAME}
// placeholder, never its value or a masked form.

// configYAMLQueryError is a query the route refuses with 400.
type configYAMLQueryError struct{ msg string }

func (e *configYAMLQueryError) Error() string { return e.msg }

func configYAMLHandler(
	cs *store.ConfigStore, controller *config.ConfigController,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := controller.Desired().Config
		keys, overrides, err := configYAMLKeys(r, cs, cfg)
		var refused *configYAMLQueryError
		switch {
		case errors.As(err, &refused):
			jsonError(w, refused.msg, http.StatusBadRequest)
			return
		case err != nil:
			slog.Error("config YAML: loading stored overrides failed", "error", err)
			jsonError(w, "failed to load stored overrides", http.StatusInternalServerError)
			return
		}
		out, err := config.EffectiveYAML(cfg, keys, store.IsSecretConfigKey)
		if err != nil {
			slog.Error("config YAML: rendering failed", "error", err)
			jsonError(w, "failed to render the configuration as YAML",
				http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]any{"yaml": string(out), "keys": keys,
			"only_overrides": overrides})
	}
}

// configYAMLKeys reads the query: the keys to render, sorted, and whether
// they are the stored overrides.
func configYAMLKeys(
	r *http.Request, cs *store.ConfigStore, cfg *config.Config,
) ([]string, bool, error) {
	q := r.URL.Query()
	known := store.ConfigReadModel(cfg)
	overrides, err := parseOverridesFlag(q.Get("overrides"))
	if err != nil {
		return nil, false, err
	}
	_, hasKeys := q["keys"]
	switch {
	case hasKeys && overrides:
		return nil, false, configYAMLRefusal("pass keys or overrides, not both")
	case hasKeys:
		keys, err := requestedConfigKeys(q.Get("keys"), known)
		return keys, false, err
	case overrides:
		keys, err := storedOverrideKeys(r, cs, known)
		return keys, true, err
	}
	return sortedConfigKeys(known), false, nil
}

func parseOverridesFlag(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	}
	return false, configYAMLRefusal("overrides must be 1 or true, got " + quotedConfigValue(raw))
}

// requestedConfigKeys splits a comma-separated key list, refusing an
// unknown key by name and an empty list.
func requestedConfigKeys(raw string, known map[string]any) ([]string, error) {
	seen := map[string]bool{}
	var keys []string
	for _, part := range strings.Split(raw, ",") {
		key := strings.TrimSpace(part)
		if key == "" || seen[key] {
			continue
		}
		if _, ok := known[key]; !ok {
			return nil, configYAMLRefusal("unknown configuration key " + quotedConfigValue(key))
		}
		seen[key] = true
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, configYAMLRefusal("keys names no configuration key")
	}
	sort.Strings(keys)
	return keys, nil
}

// storedOverrideKeys are the keys a stored global override sets that the
// runtime replays; YAML fleet mode (no store) has none.
func storedOverrideKeys(
	r *http.Request, cs *store.ConfigStore, known map[string]any,
) ([]string, error) {
	if cs == nil {
		return []string{}, nil
	}
	overrides, err := cs.GetOverrides(r.Context(), 0)
	if err != nil {
		return nil, err
	}
	keys := []string{}
	for _, o := range overrides {
		if _, ok := known[o.Key]; !ok {
			continue
		}
		if _, replayed, _ := store.ReplayOverride(o.Key, o.Value); replayed {
			keys = append(keys, o.Key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func sortedConfigKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func configYAMLRefusal(msg string) error {
	return &configYAMLQueryError{msg: msg}
}

func quotedConfigValue(v string) string {
	if len(v) > 80 {
		v = v[:80] + "..."
	}
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}
