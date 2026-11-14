package advisor

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// ioSettingsCategory is the deterministic PostgreSQL 18 I/O settings check
// (market-15). It is advice only: no finding carries SQL, and none of the
// I/O settings it names is executable.
const ioSettingsCategory = "io_settings"

// ioPre18Defaults are the stock values before PostgreSQL 18 of the I/O
// concurrency settings whose default 18 raised to 16. A value at or below
// one of them, set outside the defaults, is most likely carried over from
// an older server's configuration.
var ioPre18Defaults = map[string]int{
	"effective_io_concurrency":   1,
	"maintenance_io_concurrency": 10,
}

// pg18IOConcurrencyDefault is PostgreSQL 18's default for both settings.
const pg18IOConcurrencyDefault = 16

// databaseSources are the pg_settings sources of a value set for this
// database alone (ALTER DATABASE, ALTER ROLE ... IN DATABASE).
var databaseSources = map[string]bool{"database": true, "database user": true}

// ioSettingsCycle runs the check for one advisor cycle. The category is
// evaluated (its stale findings may resolve) only when the settings were
// read and the server's major is known. The I/O settings belong to the
// whole instance (advisors-08), so a database that leaves its instance
// settings to an owner reports only the values set for it alone.
func (a *Advisor) ioSettingsCycle(snap *collector.Snapshot, own ownership,
) ([]analyzer.Finding, bool) {
	if a.pgMajor <= 0 || snap.ConfigData == nil ||
		len(snap.ConfigData.PGSettings) == 0 {
		return nil, false
	}
	out := ioSettingsFindings(snap.ConfigData, a.pgMajor)
	if !own.defers() {
		return out, true
	}
	var mine []analyzer.Finding
	for _, f := range out {
		if f.Detail["scope"] == "database" {
			mine = append(mine, f)
		}
	}
	return mine, true
}

// ioSettingsFindings reports, on PostgreSQL 18 and later, the I/O settings
// that undo 18's asynchronous I/O: a carried-over concurrency value and
// io_method = sync. A server before 18 (or of unknown major) gets none.
func ioSettingsFindings(cd *collector.ConfigSnapshot, pgMajor int) []analyzer.Finding {
	if cd == nil || pgMajor < 18 {
		return nil
	}
	var out []analyzer.Finding
	for _, s := range cd.PGSettings {
		if f, ok := ioSettingFinding(s, pgMajor); ok {
			out = append(out, f)
		}
	}
	return out
}

// ioSettingFinding checks one setting. A value from the default source, an
// unknown source or a value that does not parse is never reported.
func ioSettingFinding(s collector.PGSetting, pgMajor int) (analyzer.Finding, bool) {
	source := strings.TrimSpace(s.Source)
	if source == "" || source == "default" {
		return analyzer.Finding{}, false
	}
	if pre, ok := ioPre18Defaults[s.Name]; ok {
		v, err := strconv.Atoi(strings.TrimSpace(s.Setting))
		if err != nil || v < 0 || v > pre {
			return analyzer.Finding{}, false
		}
		return carriedConcurrencyFinding(s, pre, pgMajor), true
	}
	if s.Name == "io_method" && strings.EqualFold(strings.TrimSpace(s.Setting), "sync") {
		return ioFinding(s, pgMajor, "io_method = sync: asynchronous I/O is switched off",
			fmt.Sprintf("Unless sync was chosen on purpose, remove io_method where it is "+
				"set (source: %s) so the default, worker, applies. The change needs a "+
				"restart; on a managed service use the provider's parameter settings.",
				source)), true
	}
	return analyzer.Finding{}, false
}

func carriedConcurrencyFinding(s collector.PGSetting, pre, pgMajor int) analyzer.Finding {
	title := fmt.Sprintf("%s = %s, a value carried over from before 18, overrides the "+
		"new default of %d", s.Name, s.Setting, pg18IOConcurrencyDefault)
	rec := fmt.Sprintf("Remove %s where it is set (source: %s), or set it for the "+
		"storage, so PostgreSQL 18's default of %d applies: at %s it switches off most "+
		"of the read-ahead that asynchronous I/O adds. A reload applies the change; on "+
		"a managed service use the provider's parameter settings.", s.Name,
		strings.TrimSpace(s.Source), pg18IOConcurrencyDefault, s.Setting)
	f := ioFinding(s, pgMajor, title, rec)
	f.Detail["pre18_default"] = strconv.Itoa(pre)
	f.Detail["pg18_default"] = strconv.Itoa(pg18IOConcurrencyDefault)
	return f
}

// ioFinding is the advice-only finding about one I/O setting.
// A value set for this database alone is about the database, not the
// instance (detail scope).
func ioFinding(s collector.PGSetting, pgMajor int, title, rec string) analyzer.Finding {
	source := strings.TrimSpace(s.Source)
	scope := "instance"
	if databaseSources[source] {
		scope = "database"
	}
	return analyzer.Finding{
		Category:         ioSettingsCategory,
		Severity:         "info",
		ObjectType:       "configuration",
		ObjectIdentifier: scope + ":" + s.Name,
		Title:            title,
		Detail: map[string]any{
			"setting":  s.Name,
			"value":    s.Setting,
			"source":   source,
			"scope":    scope,
			"pg_major": pgMajor,
			"doc_note": ioDocNote(s.Name, pgMajor),
		},
		Recommendation: rec,
	}
}

// ioDocNote is the documented version note for the setting on pgMajor: the
// advice-only docs first, then the advisor docs (effective_io_concurrency).
func ioDocNote(name string, pgMajor int) string {
	doc, ok := pgconf.AdviceOnlyDocs[name]
	if !ok {
		doc = pgconf.Docs[name]
	}
	return strings.Join(doc.NotesFor(pgMajor), " ")
}
