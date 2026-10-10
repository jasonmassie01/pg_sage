package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// AgentsConfig configures agent posture (G0, spec §6.15 and §9): which
// roles count as exposed and which client names hint at an agent. Posture
// runs in every first look, when the catalog changes and daily; it only
// reports, and every fix it proposes is a manual script (L1).
type AgentsConfig struct {
	ExposedRoles   []string            `yaml:"exposed_roles" doc:"Roles untrusted clients reach (a PostgREST anonymous role). PUBLIC is always exposed; anon and authenticated are added when both exist (Supabase). Default: none."`
	ClientPatterns []string            `yaml:"client_patterns" doc:"Case-insensitive regexes, each anchored with ^, over application_name that hint a session is an agent. Empty disables hints. Default: ^mcp ^claude ^cursor ^codex ^langgraph ^crewai."`
	Posture        AgentsPostureConfig `yaml:"posture"`
	// Roles, Broker and SingleOperatorMode are the G1 core (agents_core.go).
	Roles  AgentsRolesConfig  `yaml:"roles"`
	Broker AgentsBrokerConfig `yaml:"broker"`
	// KillVerifyTimeoutSeconds bounds the kill switch's check that no
	// agent backend remains (agents_kill.go).
	KillVerifyTimeoutSeconds int    `yaml:"kill_verify_timeout_seconds" doc:"Seconds the agent kill switch waits to verify that no agent session remains on the primary and each configured replica. 1 to 3600. Default: 10."`
	KillFallbackLog          string `yaml:"kill_fallback_log" doc:"Local file logging kill steps run without the gate or control database; copied to the action log once it is back. Default: agent-kill-fallback.log."`
	SingleOperatorMode       bool   `yaml:"single_operator_mode" doc:"Lets one person approve a widening agent change (profile, ceiling, unfreeze after a kill) with a recorded reason; each such approval enters a review queue. Default: false."`
	// ControlDatabase and DefaultEnvironment: agents_env.go (spec §6.5).
	ControlDatabase    string `yaml:"control_database" doc:"Monitored database that holds agent governance state. Required unless mode is meta; without it agent governance runs posture checks only. Default: empty."`
	DefaultEnvironment string `yaml:"default_environment" doc:"Environment of a database without a verified label. Only prod is accepted: an unverified binding is always prod. Default: prod."`
}

// AgentsPostureConfig tunes the agent posture checks.
type AgentsPostureConfig struct {
	MemoryGrowthGBDay float64 `yaml:"memory_growth_gb_day" doc:"Growth in GB per day of an agent memory store (LangGraph checkpoint tables) without deletes that posture reports. Greater than 0. Default: 5."`
	DailyAt           string  `yaml:"daily_at" doc:"Local time (HH:MM, 24-hour) of the daily posture run; it also runs in the first look and when the catalog it reads changes. Default: 03:00."`
}

// Agent posture defaults (spec §9).
const (
	DefaultPostureMemoryGrowthGBDay = 5
	DefaultPostureDailyAt           = "03:00"
)

// DefaultClientPatterns are the agent client hints of spec §9.
func DefaultClientPatterns() []string {
	return []string{"^mcp", "^claude", "^cursor", "^codex", "^langgraph", "^crewai"}
}

func defaultAgentsConfig() AgentsConfig {
	return AgentsConfig{ClientPatterns: DefaultClientPatterns(),
		DefaultEnvironment: DefaultAgentsEnvironment,
		Posture: AgentsPostureConfig{MemoryGrowthGBDay: DefaultPostureMemoryGrowthGBDay,
			DailyAt: DefaultPostureDailyAt},
		Roles: defaultAgentsRoles(), Broker: defaultAgentsBroker(),
		KillVerifyTimeoutSeconds: DefaultKillVerifyTimeoutSeconds,
		KillFallbackLog:          DefaultKillFallbackLog}
}

func (a AgentsConfig) validate() error {
	for _, r := range a.ExposedRoles {
		if err := ValidExposedRole(r); err != nil {
			return fmt.Errorf("agents.exposed_roles: %w", err)
		}
	}
	for _, p := range a.ClientPatterns {
		if err := ValidClientPattern(p); err != nil {
			return fmt.Errorf("agents.client_patterns: %w", err)
		}
	}
	if !(a.Posture.MemoryGrowthGBDay > 0) {
		return fmt.Errorf("agents.posture.memory_growth_gb_day must be greater than 0, "+
			"got %v", a.Posture.MemoryGrowthGBDay)
	}
	if _, _, err := ParseDailyAt(a.Posture.DailyAt); err != nil {
		return fmt.Errorf("agents.posture.daily_at: %w", err)
	}
	if err := a.validateCore(); err != nil {
		return err
	}
	if err := a.validateKill(); err != nil {
		return err
	}
	return a.validateEnvironment()
}

// ValidExposedRole checks one agents.exposed_roles entry: a role name,
// not PUBLIC, which is always exposed.
func ValidExposedRole(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("a role name is empty")
	}
	if strings.EqualFold(name, "public") {
		return errors.New("PUBLIC is always exposed; list only roles")
	}
	return nil
}

// ValidClientPattern checks one agents.client_patterns entry: a regular
// expression anchored at the start with ^ (a hint, not a substring search).
func ValidClientPattern(p string) error {
	if !strings.HasPrefix(p, "^") {
		return fmt.Errorf("pattern %q must be anchored with ^", p)
	}
	if _, err := regexp.Compile("(?i)" + p); err != nil {
		return fmt.Errorf("pattern %q: %w", p, err)
	}
	return nil
}

// ParseDailyAt reads a 24-hour "H:MM" or "HH:MM" local time.
func ParseDailyAt(s string) (hour, minute int, err error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) < 1 || len(h) > 2 || len(m) != 2 {
		return 0, 0, fmt.Errorf("%q is not a HH:MM time", s)
	}
	hour, herr := strconv.Atoi(h)
	minute, merr := strconv.Atoi(m)
	if herr != nil || merr != nil || h[0] == '-' || h[0] == '+' || m[0] == '-' ||
		m[0] == '+' || hour > 23 || minute > 59 {
		return 0, 0, fmt.Errorf("%q is not a HH:MM time", s)
	}
	return hour, minute, nil
}
