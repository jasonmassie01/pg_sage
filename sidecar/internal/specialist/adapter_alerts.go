package specialist

import (
	"net/http"
	"strings"
	"unicode"
)

// Shared by the alert adapters (PagerDuty, incident.io, Azure Monitor):
// KEY=database[:family] routes from configuration, and the answer for an
// alert whose source no route names.

// CodeUnknownDatabase marks an ignored alert whose source is not mapped
// to a database: the operator adds a route to the setting the reason
// names.
const CodeUnknownDatabase = "unknown_database"

// maxRouteKey bounds a route key (an Azure resource id is the longest).
const maxRouteKey = 512

// ParseAlertRoutes parses KEY=database[:family] entries of setting. With
// fold, keys are matched without case (lower-cased here and at lookup).
func ParseAlertRoutes(setting string, entries []string, fold bool) (map[string]PagerDutyRoute,
	error) {
	return parseRoutes(setting, entries, fold, routeKeyOK)
}

func routeKeyOK(key string) bool {
	return key != "" && len(key) <= maxRouteKey && strings.IndexFunc(key, unicode.IsControl) < 0
}

func parseRoutes(setting string, entries []string, fold bool,
	keyOK func(string) bool) (map[string]PagerDutyRoute, error) {
	out := make(map[string]PagerDutyRoute, len(entries))
	for _, entry := range entries {
		key, target, ok := strings.Cut(entry, "=")
		key, target = strings.TrimSpace(key), strings.TrimSpace(target)
		if fold {
			key = strings.ToLower(key)
		}
		database, family, _ := strings.Cut(target, ":")
		switch {
		case !ok || !keyOK(key):
			return nil, invalidf("%s entry %q: want KEY=database[:family]", setting, entry)
		case !databaseName.MatchString(database):
			return nil, invalidf("%s entry %q: invalid database", setting, entry)
		case family != "" && families[family] == "":
			return nil, invalidf("%s entry %q: unknown family %q", setting, entry, family)
		}
		if _, dup := out[key]; dup {
			return nil, invalidf("%s maps %q twice", setting, key)
		}
		out[key] = PagerDutyRoute{Database: database, Family: family}
	}
	return out, nil
}

// ignoreUnmapped answers an alert no route names; the reason names the
// setting to add one to.
func ignoreUnmapped(w http.ResponseWriter, source, setting string) {
	writeJSON(w, http.StatusAccepted, WebhookIgnored{ContractVersion: ContractVersion,
		Ignored: true, Code: CodeUnknownDatabase, Reason: source + " is not mapped to a " +
			"database; add it to " + setting})
}

func ignoreEvent(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusAccepted, WebhookIgnored{ContractVersion: ContractVersion,
		Ignored: true, Reason: reason})
}
