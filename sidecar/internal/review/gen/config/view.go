package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/pgconf"
)

// Evidence ids outside the settings section the rules cite.
const memoryFactID = "provider/memory_bytes"

// settingName is a setting name as pg_settings prints it (a custom
// setting has one dot).
var settingName = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)?$`)

// firstNumber is the first run of digits of a version string.
var firstNumber = regexp.MustCompile(`[0-9]+`)

// view is the pack as the rules read it.
type view struct {
	settings map[string]workpack.SettingFact
	order    []workpack.SettingFact // by name, valid names only
	skipped  []string               // names that are not setting names
	measures workpack.Measures
	memory   float64 // the provider's memory in bytes, when memoryOK
	memoryOK bool
	platform string
	major    int // the server's major version, 0 when unknown
	conns    []workpack.ConnectionHour
}

// readView decodes the settings section (ok false when the pack has none),
// the provider's memory fact, the header and the connections section.
func readView(pack *workpack.Pack) (view, bool, error) {
	raw := pack.File("settings.json")
	if raw == nil {
		return view{}, false, nil
	}
	var sec workpack.Settings
	if err := json.Unmarshal(raw, &sec); err != nil {
		return view{}, false, fmt.Errorf("decode settings.json: %w", err)
	}
	h, err := pack.Header()
	if err != nil {
		return view{}, false, err
	}
	v := view{settings: map[string]workpack.SettingFact{}, measures: sec.Measures,
		platform: h.Platform, major: parseMajor(h.PGVersion)}
	for _, s := range sec.Settings {
		if !settingName.MatchString(s.Name) {
			v.skipped = append(v.skipped, s.Name)
			continue
		}
		v.settings[s.Name] = s
		v.order = append(v.order, s)
	}
	sort.Slice(v.order, func(a, b int) bool { return v.order[a].Name < v.order[b].Name })
	if f, err := workpack.Resolve(pack, memoryFactID); err == nil {
		var m any
		if json.Unmarshal(f.Value, &m) == nil {
			if pf, ok := m.(map[string]any); ok {
				v.memory, v.memoryOK = positive(pf["value"])
			}
		}
	}
	if raw := pack.File("connections.json"); raw != nil {
		var c workpack.Connections
		if err := json.Unmarshal(raw, &c); err != nil {
			return view{}, false, fmt.Errorf("decode connections.json: %w", err)
		}
		v.conns = c.Hours
	}
	return v, true, nil
}

// positive is x as a number above zero.
func positive(x any) (float64, bool) {
	n, ok := x.(float64)
	return n, ok && n > 0
}

// measure is a measures value as a number ("unknown" is not).
func measure(x any) (float64, bool) {
	n, ok := x.(float64)
	return n, ok && n >= 0
}

// parseMajor reads the major version from "17.2", "18beta1",
// "PostgreSQL 16.4 on ..." or a server_version_num such as 170002.
func parseMajor(s string) int {
	digits := firstNumber.FindString(s)
	n, err := strconv.Atoi(digits)
	switch {
	case err != nil:
		return 0
	case n >= 10000:
		return n / 10000
	}
	return n
}

// docFor is how the rules parse a setting fact: bytes for memory,
// milliseconds for time, a plain number otherwise, from pg_settings' own
// unit.
func docFor(s workpack.SettingFact) pgconf.GUCDoc {
	switch s.Unit {
	case "B", "kB", "8kB", "MB":
		return pgconf.GUCDoc{Unit: "bytes", BaseUnit: s.Unit}
	case "ms", "s", "min":
		return pgconf.GUCDoc{Unit: "ms", BaseUnit: s.Unit}
	}
	return pgconf.GUCDoc{Unit: "count"}
}

// value is the named setting's value: bytes for memory, milliseconds for
// time, the number otherwise. ok is false when the setting is absent or
// its value does not parse.
func (v view) value(name string) (float64, bool) {
	s, ok := v.settings[name]
	if !ok {
		return 0, false
	}
	n, err := pgconf.ParseValue(s.Value, docFor(s))
	return n, err == nil
}

// cite is the evidence ids of the named settings that are in the pack.
func (v view) cite(names ...string) []string {
	var out []string
	for _, n := range names {
		if s, ok := v.settings[n]; ok {
			out = append(out, s.ID)
		}
	}
	return out
}
