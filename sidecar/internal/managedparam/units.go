package managedparam

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/pgconf"
)

// RDS parameter groups and Cloud SQL flags take a setting in its
// PostgreSQL base unit (pg_settings.unit) without a suffix: shared_buffers
// in 8kB pages, work_mem in kB, autovacuum_naptime in seconds.

var (
	plainNumber = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	keyword     = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)
)

// booleanParams are boolean settings whose value is respelled per
// provider (RDS uses 1/0, Cloud SQL on/off).
var booleanParams = map[string]bool{
	"jit": true, "autovacuum": true, "track_io_timing": true, "log_lock_waits": true,
	"log_temp_files_enabled": true, "track_activities": true, "track_counts": true,
	"log_checkpoints": true, "log_connections": true, "log_disconnections": true,
	"enable_partitionwise_join": true, "enable_partitionwise_aggregate": true,
	"hot_standby_feedback": true, "wal_compression": true,
}

// ProviderValue converts value into the provider's form for parameter and
// returns it with its unit ("" for unitless settings).
func ProviderValue(provider, parameter, value string) (string, string, error) {
	provider = NormalizeProvider(provider)
	value = strings.TrimSpace(value)
	if !intentValuePattern.MatchString(value) {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidValue, value)
	}
	if booleanParams[parameter] {
		v, err := providerBool(provider, value)
		return v, "", err
	}
	doc, documented := pgconf.Docs[parameter]
	if documented && (doc.Unit == "bytes" || doc.Unit == "ms") {
		return baseUnitValue(parameter, value, doc)
	}
	if plainNumber.MatchString(value) {
		if documented {
			if ok, why := pgconf.ValidateValue(parameter, value); !ok {
				return "", "", fmt.Errorf("%w: %s", ErrInvalidValue, why)
			}
		}
		return value, "", nil
	}
	if keyword.MatchString(value) {
		return value, "", nil
	}
	return "", "", fmt.Errorf("%w: %q for %s has a unit pg_sage cannot convert", ErrInvalidValue,
		value, parameter)
}

func providerBool(provider, value string) (string, error) {
	var on bool
	switch strings.ToLower(value) {
	case "on", "true", "1", "yes":
		on = true
	case "off", "false", "0", "no":
	default:
		return "", fmt.Errorf("%w: %q is not a boolean", ErrInvalidValue, value)
	}
	if provider == "cloud-sql" {
		if on {
			return "on", nil
		}
		return "off", nil
	}
	if on {
		return "1", nil
	}
	return "0", nil
}

// baseUnitValue converts a memory or time value into a whole number of the
// parameter's base unit, refusing values outside the documented range and
// values that are not a whole number of base units.
func baseUnitValue(parameter, value string, doc pgconf.GUCDoc) (string, string, error) {
	if ok, why := pgconf.ValidateValue(parameter, value); !ok {
		return "", "", fmt.Errorf("%w: %s", ErrInvalidValue, why)
	}
	base := doc.BaseUnit
	if base == "" && doc.Unit == "ms" {
		base = "ms"
	}
	n, err := pgconf.ToBaseUnits(value, base)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return "", "", fmt.Errorf("%w: %q for %s", ErrInvalidValue, value, parameter)
	}
	if n != math.Trunc(n) || n > math.MaxInt64/2 {
		return "", "", fmt.Errorf("%w: %q is not a whole number of %s", ErrInvalidValue,
			value, base)
	}
	return strconv.FormatInt(int64(n), 10), base, nil
}
