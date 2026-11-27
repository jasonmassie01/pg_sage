package config

import (
	"strconv"
	"strings"
)

// MaxIntSetting is the ceiling of every integer API type (registry.go);
// YAML and the override API apply it through the same KeySpec (config-02b).
const MaxIntSetting = 1 << 30

// ParseBool is the one boolean grammar for string-valued settings: the
// override API's validator, every hot-reload setter and the read model use
// it, so a value that validates is the value that applies. It is
// strconv.ParseBool on lowercased input: true, t, 1, false, f and 0 in any
// case.
func ParseBool(value string) (bool, error) {
	return strconv.ParseBool(strings.ToLower(value))
}
