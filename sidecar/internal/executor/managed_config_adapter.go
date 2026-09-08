package executor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	ErrManagedConfigAdapterUnavailable = errors.New("managed config adapter unavailable")
	ErrManagedConfigNotEffective       = errors.New("managed config is not effective")
	ErrManagedConfigUnsupported        = errors.New("managed parameter is provider-controlled")
)

type ManagedConfigMechanism string

const (
	ManagedParameterGroup  ManagedConfigMechanism = "parameter_group"
	ManagedDatabaseFlag    ManagedConfigMechanism = "database_flag"
	ManagedServerParameter ManagedConfigMechanism = "server_parameter"
	ManagedProviderConfig  ManagedConfigMechanism = "provider_config"
)

type ManagedConfigChange struct {
	Provider  string
	Mechanism ManagedConfigMechanism
	Parameter string
	Value     string
	Reset     bool
}

type ManagedConfigResult struct {
	InEffect bool
	Note     string
}

type ManagedConfigAdapter interface {
	ApplyParameter(context.Context, ManagedConfigChange) (ManagedConfigResult, error)
}

var managedConfigValuePattern = regexp.MustCompile(`^[0-9]+(?:[kKmMgGtT][bB])?$`)

func (e *Executor) applyManagedCustodianConfig(
	ctx context.Context, proposal CustodianProposal,
) (ManagedConfigResult, bool, error) {
	provider := strings.ToLower(strings.TrimSpace(e.cfg.CloudEnvironment))
	if !isManagedProvider(provider) || !isAlterSystem(proposal.SQL) {
		return ManagedConfigResult{}, false, nil
	}
	change, err := parseManagedConfigChange(provider, proposal.SQL)
	if err != nil {
		return ManagedConfigResult{}, true, err
	}
	if provider == "neon" {
		return ManagedConfigResult{}, true, fmt.Errorf("%w: %s",
			ErrManagedConfigUnsupported, managedConfigGuidance(provider, change.Parameter))
	}
	e.policyMu.RLock()
	adapter := e.managedConfig
	e.policyMu.RUnlock()
	if adapter == nil {
		return ManagedConfigResult{}, true, fmt.Errorf(
			"%w for provider %q", ErrManagedConfigAdapterUnavailable, provider)
	}
	result, err := adapter.ApplyParameter(ctx, change)
	if err != nil {
		return result, true, fmt.Errorf("apply provider parameter: %w", err)
	}
	if !result.InEffect {
		return result, true, fmt.Errorf("%w: %s", ErrManagedConfigNotEffective, result.Note)
	}
	return result, true, nil
}

func parseManagedConfigChange(provider, sql string) (ManagedConfigChange, error) {
	if err := ValidateExecutorSQL(sql); err != nil {
		return ManagedConfigChange{}, fmt.Errorf("validate managed config SQL: %w", err)
	}
	const prefix = "ALTER SYSTEM SET "
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sql), ";"))
	if !strings.HasPrefix(strings.ToUpper(trimmed), prefix) {
		return ManagedConfigChange{}, fmt.Errorf("managed config requires ALTER SYSTEM SET")
	}
	assignment := strings.TrimSpace(trimmed[len(prefix):])
	parts := strings.SplitN(assignment, "=", 2)
	if len(parts) != 2 {
		return ManagedConfigChange{}, fmt.Errorf("managed config SET requires a value")
	}
	parameter := strings.ToLower(strings.TrimSpace(parts[0]))
	if parameter != "max_slot_wal_keep_size" {
		return ManagedConfigChange{}, fmt.Errorf("unsupported managed parameter %q", parameter)
	}
	value, err := parseManagedConfigValue(parts[1])
	if err != nil {
		return ManagedConfigChange{}, err
	}
	return ManagedConfigChange{
		Provider: provider, Mechanism: managedConfigMechanism(provider),
		Parameter: parameter, Value: value,
	}, nil
}

func parseManagedConfigValue(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '\'' || raw[len(raw)-1] != '\'' {
		return "", fmt.Errorf("managed config value must be a quoted literal")
	}
	value := raw[1 : len(raw)-1]
	if !managedConfigValuePattern.MatchString(value) {
		return "", fmt.Errorf("invalid managed config value %q", value)
	}
	digits := strings.TrimRight(value, "kKmMgGtTbB")
	amount, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || amount == 0 {
		return "", fmt.Errorf("managed config value must be greater than zero")
	}
	return value, nil
}

func managedConfigMechanism(provider string) ManagedConfigMechanism {
	switch provider {
	case "neon", "supabase":
		return ManagedProviderConfig
	case "cloud-sql", "cloudsql", "gcp", "alloydb":
		return ManagedDatabaseFlag
	case "azure", "azure-flexible", "azure-single":
		return ManagedServerParameter
	default:
		return ManagedParameterGroup
	}
}

func managedConfigGuidance(provider, parameter string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "neon":
		return "Neon controls instance parameter " + parameter +
			"; only permitted session, database, or role settings can be changed through SQL"
	case "supabase":
		return "Supabase instance parameter " + parameter +
			" requires a supported Postgres config control through the Supabase API/CLI; " +
			"verify availability and effective state; ALTER SYSTEM is unavailable"
	default:
		return "managed provider (" + provider + "): apply " + parameter +
			" via the provider parameter group / database flags; ALTER SYSTEM is unavailable"
	}
}
