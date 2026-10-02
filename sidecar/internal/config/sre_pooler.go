package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// SREPoolerConfig is one external connection pooler whose telemetry
// connection investigations read (CHECK-04): PgBouncer's admin console,
// SHOW POOLS and SHOW STATS only, read-only and bounded. The DSN is a
// secret: set it through ${ENV} substitution or dsn_file.
type SREPoolerConfig struct {
	Name      string   `yaml:"name" doc:"Pooler name shown in investigations (letters, digits, '.', '_', '-'; unique)."`
	DSN       string   `yaml:"dsn" secret:"true" doc:"PgBouncer admin-console DSN of a stats_users user (database pgbouncer). Prefer ${ENV} substitution or dsn_file; never logged."`
	DSNFile   string   `yaml:"dsn_file" doc:"File holding the admin-console DSN (e.g. a mounted secret), read at startup. Exclusive with dsn."`
	Databases []string `yaml:"databases" doc:"Monitored databases (instance names) this pooler fronts; empty = every database of this sidecar."`
	Pools     []string `yaml:"pools" doc:"PgBouncer databases (pools) to report; empty = every pool except the admin console."`
	TimeoutMS int      `yaml:"timeout_ms" doc:"Time allowed to connect and read one pooler, 100-2000 ms. Default: 1000."`
}

// DefaultSREPoolerTimeoutMS bounds one pooler read by default.
const DefaultSREPoolerTimeoutMS = 1000

var poolerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// Timeout is the time allowed to read the pooler.
func (p SREPoolerConfig) Timeout() time.Duration {
	return time.Duration(p.TimeoutMS) * time.Millisecond
}

// Fronts reports whether the pooler fronts the named database.
func (p SREPoolerConfig) Fronts(database string) bool {
	return len(p.Databases) == 0 || (database != "" && slices.Contains(p.Databases, database))
}

// ResolveDSN returns the DSN, reading dsn_file when set. Errors name the
// pooler and the file, never the DSN.
func (p SREPoolerConfig) ResolveDSN() (string, error) {
	if p.DSNFile == "" {
		return p.DSN, nil
	}
	raw, err := os.ReadFile(p.DSNFile)
	if err != nil {
		return "", fmt.Errorf("sre.poolers %q dsn_file %s: %w", p.Name, p.DSNFile, err)
	}
	dsn := strings.TrimSpace(string(raw))
	if dsn == "" {
		return "", fmt.Errorf("sre.poolers %q dsn_file %s is empty", p.Name, p.DSNFile)
	}
	return dsn, nil
}

// applyPoolerDefaults fills an unset timeout.
func applyPoolerDefaults(ps []SREPoolerConfig) {
	for i := range ps {
		if ps[i].TimeoutMS == 0 {
			ps[i].TimeoutMS = DefaultSREPoolerTimeoutMS
		}
	}
}

// validatePoolers fills unset timeouts in place (the slice is the loaded
// config's) and checks every pooler. Errors never quote a DSN.
func validatePoolers(ps []SREPoolerConfig) error {
	applyPoolerDefaults(ps)
	seen := map[string]bool{}
	for i, p := range ps {
		if err := validatePooler(p, seen); err != nil {
			return fmt.Errorf("sre.poolers[%d]: %w", i, err)
		}
	}
	return nil
}

func validatePooler(p SREPoolerConfig, seen map[string]bool) error {
	switch {
	case !poolerNamePattern.MatchString(p.Name):
		return errors.New("name must be 1-63 letters, digits, '.', '_' or '-'")
	case seen[p.Name]:
		return fmt.Errorf("duplicate name %q", p.Name)
	case p.DSN == "" && p.DSNFile == "":
		return fmt.Errorf("%q needs dsn or dsn_file", p.Name)
	case p.DSN != "" && p.DSNFile != "":
		return fmt.Errorf("%q: dsn and dsn_file are exclusive", p.Name)
	case p.TimeoutMS < 100 || p.TimeoutMS > 2000:
		return fmt.Errorf("%q timeout_ms must be 100-2000, got %d", p.Name, p.TimeoutMS)
	case len(p.Pools) > 100 || slices.Contains(p.Pools, ""):
		return fmt.Errorf("%q pools must be at most 100 non-empty names", p.Name)
	case slices.Contains(p.Databases, ""):
		return fmt.Errorf("%q databases must be non-empty names", p.Name)
	}
	seen[p.Name] = true
	return nil
}
