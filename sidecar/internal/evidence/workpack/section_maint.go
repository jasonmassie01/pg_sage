package workpack

import (
	"sort"
	"strings"
	"time"
)

// settings.json and autovacuum.json from the history's picks and edges.

// settingMeasures names the measure each judged setting is read beside.
var settingMeasures = map[string]string{
	"work_mem":        "temp_bytes_per_hour",
	"max_wal_size":    "checkpoints_requested, checkpoints_timed, wal_bytes_per_hour",
	"max_connections": "peak_client_backends, usable_connections",
}

// buildSettings is the last pick's settings with the window's changes.
func buildSettings(picks []settingsPick, m Measures, hw Hardware) Settings {
	out := Settings{Status: valueUnknown, Settings: []SettingFact{}, Measures: m,
		Hardware: hw}
	if len(picks) == 0 {
		return out
	}
	out.Status = relStatusOK
	prev := map[string]string{}
	changes := map[string][]SettingChange{}
	for i, p := range picks {
		for _, s := range p.settings {
			if was, ok := prev[s.Name]; ok && i > 0 && was != s.Setting {
				changes[s.Name] = append(changes[s.Name], SettingChange{Day: p.day,
					From: was, To: s.Setting})
			}
			prev[s.Name] = s.Setting
		}
	}
	for _, s := range picks[len(picks)-1].settings {
		ch := changes[s.Name]
		if ch == nil {
			ch = []SettingChange{}
		}
		out.Settings = append(out.Settings, SettingFact{ID: "setting/" + s.Name,
			Name: s.Name, Value: s.Setting, Unit: s.Unit, Source: s.Source, Changes: ch,
			Measure: settingMeasures[s.Name]})
	}
	sort.Slice(out.Settings, func(a, b int) bool {
		return out.Settings[a].Name < out.Settings[b].Name
	})
	return out
}

// hardwareOf is the vCPUs and memory of the provider row, or unknown.
func hardwareOf(row *hostRow) Hardware {
	hw := Hardware{VCPUs: valueUnknown, MemoryBytes: valueUnknown}
	if row == nil {
		return hw
	}
	if row.vcpus > 0 {
		hw.VCPUs = row.vcpus
	}
	if row.mem > 0 {
		hw.MemoryBytes = row.mem
	}
	return hw
}

// buildAutovacuum is each table of the window's last tables snapshot.
func buildAutovacuum(win relWindow, days []string,
	dead map[string]map[string]int64) Autovacuum {
	names := make([]string, 0, len(win.tabLast))
	for name := range win.tabLast {
		names = append(names, name)
	}
	sort.Strings(names)
	out := Autovacuum{Tables: []AutovacuumFact{}, Total: len(names), Days: days}
	if len(names) > MaxSchemaTables {
		names, out.Truncated = names[:MaxSchemaTables], true
	}
	for _, name := range names {
		last := win.tabLast[name]
		f := AutovacuumFact{ID: "table/av/" + name, Table: name, Autovacuums: valueUnknown,
			Autoanalyzes: valueUnknown, LastAutovacuum: instantOr(last.lastAV),
			LastAutoanalyze: instantOr(last.lastAA)}
		if first, ok := win.tabFirst[name]; ok && win.status == relStatusOK {
			if last.av >= first.av {
				f.Autovacuums = last.av - first.av
			}
			if last.aa >= first.aa {
				f.Autoanalyzes = last.aa - first.aa
			}
		}
		f.Reloptions, f.ReloptionsText = parseReloptions(last.reloptions)
		f.DeadTuples = make([]*int64, len(days))
		for i, d := range days {
			if n, ok := dead[name][d]; ok {
				f.DeadTuples[i] = &n
			}
		}
		out.Tables = append(out.Tables, f)
	}
	return out
}

// instantOr is t in RFC 3339 UTC, or absent (never happened).
func instantOr(t *time.Time) any {
	if t == nil {
		return valueAbsent
	}
	return t.UTC().Format(time.RFC3339)
}

// parseReloptions splits name=value storage parameters; one that does not
// parse is kept as written.
func parseReloptions(opts []string) (map[string]string, []string) {
	kv, text := map[string]string{}, []string{}
	for _, o := range opts {
		name, value, ok := strings.Cut(o, "=")
		if !ok || name == "" {
			text = append(text, o)
			continue
		}
		kv[name] = value
	}
	return kv, text
}

// dayNames are the header days' dates.
func dayNames(days []Day) []string {
	out := make([]string, len(days))
	for i, d := range days {
		out[i] = d.Day
	}
	return out
}
