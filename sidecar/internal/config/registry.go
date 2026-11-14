package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// KeySpec is one setting's value contract, read from the struct tags of
// Config (config-02b). The YAML loader, the configuration API and the
// dashboard's config_meta.json all read it, so a range is written once:
//
//	api:"int_pos"         the API validation type; the key is overridable
//	range:"0,3600000"     inclusive bounds; they replace the type's bounds
//	enum:"stdio|http"     the accepted strings
//	zero:"default"        a file may also hold 0, meaning "use the default";
//	                      the API refuses 0 (removing the override restores it)
type KeySpec struct {
	Key         string
	Type        string // the API validation type; "" for a YAML-only setting
	Overridable bool   // the configuration API may store an override
	Integer     bool   // the value is a whole number
	HasRange    bool
	Min, Max    float64
	ZeroDefault bool
	Enum        []string
	index       []int
}

// apiType is what a validation type implies before a field's own tags.
type apiType struct {
	ranged, integer, enum bool
	min, max              float64
}

// apiTypes are the validation types the configuration API accepts.
var apiTypes = map[string]apiType{
	"int_pos":    {ranged: true, integer: true, min: 1, max: MaxIntSetting},
	"int_nonneg": {ranged: true, integer: true, min: 0, max: MaxIntSetting},
	"int_min5":   {ranged: true, integer: true, min: 5, max: MaxIntSetting},
	"int_week_minutes": {ranged: true, integer: true,
		min: 1, max: MaxInactiveSlotMinMinutes},
	"int_class_timeout": {ranged: true, integer: true,
		min: MinClassTimeoutSeconds, max: MaxClassTimeoutSeconds},
	"int_vacuum_stall": {ranged: true, integer: true,
		min: MinVacuumStallMinutes, max: MaxVacuumStallMinutes},
	"pct":                {ranged: true, integer: true, min: 0, max: 100},
	"pct1_100":           {ranged: true, integer: true, min: 1, max: 100},
	"float01":            {ranged: true, min: 0, max: 1},
	"float_share":        {ranged: true, min: 0.01, max: 1},
	"float_pos":          {ranged: true, min: 0.001, max: 1e9},
	"float_nonneg":       {ranged: true, min: 0, max: 1e9},
	"float_pct_pos":      {ranged: true, min: 0.001, max: 100},
	"bool":               {},
	"string":             {},
	"maintenance_window": {},
	"rule_list":          {},
	"trust_level":        {enum: true},
	"policy_profile":     {enum: true},
	"clone_provider":     {enum: true},
	"mcp_transport":      {enum: true},
	"llm_auth":           {enum: true},
}

var registryOnce = sync.OnceValue(func() []KeySpec {
	return mustBuildRegistry(reflect.TypeOf(Config{}))
})

var registryIndex = sync.OnceValue(func() map[string]KeySpec {
	specs := Registry()
	m := make(map[string]KeySpec, len(specs))
	for _, s := range specs {
		m[s.Key] = s
	}
	return m
})

// Registry returns every Config setting with a tag-declared contract, in
// struct order. It is built once; callers must not modify the slices.
func Registry() []KeySpec {
	return registryOnce()
}

// LookupKey returns the contract of one dot-notation key.
func LookupKey(key string) (KeySpec, bool) {
	s, ok := registryIndex()[key]
	return s, ok
}

// mustBuildRegistry panics on a malformed tag: tags are code, and a test
// that calls Registry() catches them before a binary ships.
func mustBuildRegistry(rt reflect.Type) []KeySpec {
	specs, err := buildRegistry(rt, "", nil)
	if err != nil {
		panic("config registry: " + err.Error())
	}
	return specs
}

func buildRegistry(rt reflect.Type, prefix string, index []int) ([]KeySpec, error) {
	var out []KeySpec
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		name := strings.SplitN(sf.Tag.Get("yaml"), ",", 2)[0]
		if !sf.IsExported() || name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		idx := append(append([]int(nil), index...), i)
		ft := sf.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			nested, err := buildRegistry(ft, key, idx)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			continue
		}
		// A pointer leaf keeps its Pointer kind, so a range or enum tag on it
		// is refused: validateRegistry could not read the value it bounds.
		spec, ok, err := specFromTags(key, sf, sf.Type.Kind())
		if err != nil {
			return nil, err
		}
		if ok {
			spec.index = idx
			out = append(out, spec)
		}
	}
	return out, nil
}

// specFromTags reads one field's api, range, enum and zero tags; ok is
// false for a field with none of them.
func specFromTags(key string, sf reflect.StructField, kind reflect.Kind) (KeySpec, bool, error) {
	s := KeySpec{Key: key}
	tagged := false
	if t, found := sf.Tag.Lookup("api"); found {
		if err := s.applyAPIType(t, kind); err != nil {
			return s, false, err
		}
		tagged = true
	}
	if r, found := sf.Tag.Lookup("range"); found {
		if err := s.applyRange(r, kind); err != nil {
			return s, false, err
		}
		tagged = true
	}
	if e, found := sf.Tag.Lookup("enum"); found {
		if err := s.applyEnum(e, kind); err != nil {
			return s, false, err
		}
		tagged = true
	}
	if z, found := sf.Tag.Lookup("zero"); found {
		if z != "default" || !s.HasRange || s.Min <= 0 {
			return s, false, fmt.Errorf(`%s: zero:%q needs the value "default" and `+
				"a range whose minimum is above 0", key, z)
		}
		s.ZeroDefault, tagged = true, true
	}
	if t, known := apiTypes[s.Type]; known && t.enum && len(s.Enum) == 0 {
		return s, false, fmt.Errorf("%s: api type %s needs an enum tag", key, s.Type)
	}
	return s, tagged, nil
}

func (s *KeySpec) applyAPIType(name string, kind reflect.Kind) error {
	t, ok := apiTypes[name]
	if !ok {
		return fmt.Errorf("%s: unknown api type %q", s.Key, name)
	}
	if t.ranged && !isNumberKind(kind, t.integer) {
		return fmt.Errorf("%s: api type %s does not fit a %s field", s.Key, name, kind)
	}
	s.Type, s.Overridable = name, true
	s.HasRange, s.Integer, s.Min, s.Max = t.ranged, t.integer, t.min, t.max
	return nil
}

func (s *KeySpec) applyRange(tag string, kind reflect.Kind) error {
	parts := strings.Split(tag, ",")
	if len(parts) != 2 {
		return fmt.Errorf(`%s: range:%q must be "min,max"`, s.Key, tag)
	}
	lo, errLo := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	hi, errHi := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if errLo != nil || errHi != nil || lo > hi {
		return fmt.Errorf(`%s: range:%q must be two numbers, min <= max`, s.Key, tag)
	}
	integer := isNumberKind(kind, true)
	if !integer && !isNumberKind(kind, false) {
		return fmt.Errorf("%s: range tag on a %s field", s.Key, kind)
	}
	if integer && (lo != float64(int64(lo)) || hi != float64(int64(hi))) {
		return fmt.Errorf("%s: range:%q must be whole numbers on an integer field", s.Key, tag)
	}
	s.HasRange, s.Integer, s.Min, s.Max = true, integer, lo, hi
	return nil
}

func (s *KeySpec) applyEnum(tag string, kind reflect.Kind) error {
	if tag == "" || kind != reflect.String {
		return fmt.Errorf("%s: enum:%q must list values of a string field", s.Key, tag)
	}
	seen := map[string]bool{}
	for _, v := range strings.Split(tag, "|") {
		if seen[v] {
			return fmt.Errorf("%s: enum:%q lists %q twice", s.Key, tag, v)
		}
		seen[v] = true
		s.Enum = append(s.Enum, v)
	}
	return nil
}

// isNumberKind reports whether kind holds integers (integer true) or
// floats (integer false).
func isNumberKind(kind reflect.Kind, integer bool) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return integer
	case reflect.Float32, reflect.Float64:
		return !integer
	}
	return false
}
