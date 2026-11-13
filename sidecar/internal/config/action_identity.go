package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// ErrActionDSNTarget refuses an action_dsn that is not a second role on
// the observation connection's own database (security-09).
var ErrActionDSNTarget = errors.New("action_dsn must be another role on the same database")

// ValidateActionDSN checks that the action identity's DSN reaches the same
// host, port and database as the observation connection under a different
// role. An empty action DSN is the single-identity default. Errors never
// carry either DSN, only the fields that differ.
func ValidateActionDSN(observation, action string) error {
	if strings.TrimSpace(action) == "" {
		return nil
	}
	act, err := pgconn.ParseConfig(action)
	if err != nil {
		return fmt.Errorf("%w: action_dsn does not parse as a connection string",
			ErrActionDSNTarget)
	}
	obs, err := pgconn.ParseConfig(observation)
	if err != nil {
		return fmt.Errorf("%w: the observation connection does not parse",
			ErrActionDSNTarget)
	}
	switch {
	case !strings.EqualFold(obs.Host, act.Host):
		return fmt.Errorf("%w: host %q is not the observation host %q",
			ErrActionDSNTarget, act.Host, obs.Host)
	case obs.Port != act.Port:
		return fmt.Errorf("%w: port %d is not the observation port %d",
			ErrActionDSNTarget, act.Port, obs.Port)
	case obs.Database != act.Database:
		return fmt.Errorf("%w: database %q is not the observation database %q",
			ErrActionDSNTarget, act.Database, obs.Database)
	case obs.User == act.User:
		return fmt.Errorf("%w: role %q is the observation role; give the action "+
			"identity its own role", ErrActionDSNTarget, act.User)
	}
	return nil
}

// validateActionIdentity checks every configured action_dsn: the
// postgres one in standalone mode, each database's in fleet mode.
func (c *Config) validateActionIdentity() error {
	if c.Postgres.ActionDSN != "" {
		if err := ValidateActionDSN(c.Postgres.DSN(), c.Postgres.ActionDSN); err != nil {
			return fmt.Errorf("postgres.action_dsn: %w", err)
		}
	}
	if c.Mode != "fleet" {
		return nil
	}
	for i, d := range c.Databases {
		if err := ValidateActionDSN(d.ConnString(), d.ActionDSN); err != nil {
			return fmt.Errorf("databases[%d] (%s).action_dsn: %w", i, d.Name, err)
		}
	}
	return nil
}
