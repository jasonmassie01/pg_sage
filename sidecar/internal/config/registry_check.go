package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// CheckNumber refuses n, naming the key, when it is outside the bounds. A
// zero:"default" key also accepts 0, which a file uses to leave the
// setting at its default.
func (s KeySpec) CheckNumber(n float64) error {
	if s.ZeroDefault && n == 0 {
		return nil
	}
	return s.CheckOverride(n)
}

// CheckOverride is CheckNumber for a stored API override: 0 is refused
// even on a zero:"default" key, because removing the override (DELETE)
// is how the API returns a key to its default.
func (s KeySpec) CheckOverride(n float64) error {
	if !s.HasRange || (n >= s.Min && n <= s.Max) {
		return nil
	}
	note := ""
	if s.ZeroDefault && n == 0 {
		note = " (0 is the file default: remove the override to restore it)"
	}
	return fmt.Errorf("%s must be %s-%s, got %s%s",
		s.Key, s.formatNumber(s.Min), s.formatNumber(s.Max), s.formatNumber(n), note)
}

// CheckEnum refuses v, naming the key, when the key lists its values and
// v is not one of them.
func (s KeySpec) CheckEnum(v string) error {
	if len(s.Enum) == 0 {
		return nil
	}
	for _, e := range s.Enum {
		if v == e {
			return nil
		}
	}
	shown := make([]string, len(s.Enum))
	for i, e := range s.Enum {
		shown[i] = e
		if e == "" {
			shown[i] = `""`
		}
	}
	return fmt.Errorf("%s must be one of %s, got %q", s.Key, strings.Join(shown, ", "), v)
}

func (s KeySpec) formatNumber(n float64) string {
	if s.Integer {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'g', -1, 64)
}

// validateRegistry applies every tag-declared range and enum to the loaded
// file: the same contract the configuration API applies on save. validate
// runs it after the hand-written checks, so their messages come first.
func (c *Config) validateRegistry() error {
	root := reflect.ValueOf(c).Elem()
	for _, s := range Registry() {
		if !s.HasRange && len(s.Enum) == 0 {
			continue
		}
		v, err := root.FieldByIndexErr(s.index)
		if err != nil {
			continue // an unset optional section holds no value to check
		}
		if err := s.checkValue(v); err != nil {
			return err
		}
	}
	return nil
}

func (s KeySpec) checkValue(v reflect.Value) error {
	switch {
	case v.CanInt():
		return s.CheckNumber(float64(v.Int()))
	case v.CanUint():
		return s.CheckNumber(float64(v.Uint()))
	case v.CanFloat():
		return s.CheckNumber(v.Float())
	case v.Kind() == reflect.String:
		return s.CheckEnum(v.String())
	}
	return nil
}
