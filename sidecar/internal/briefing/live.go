package briefing

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/config"
)

// liveSettings is one immutable briefing configuration with its parsed
// schedule (llm-06). The worker swaps the whole value, so a reader never
// sees a schedule from one config and channels from another.
type liveSettings struct {
	cfg      config.BriefingConfig
	schedule cronSchedule
}

// ValidateSchedule checks a briefing.schedule cron expression. Empty is
// valid and turns scheduled briefings off.
func ValidateSchedule(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return nil
	}
	_, err := parseCron(expr)
	return err
}

// Reconfigure swaps the worker's schedule, channels and webhook. An invalid
// schedule is refused and the worker keeps its settings.
func (w *Worker) Reconfigure(bc config.BriefingConfig) error {
	s, err := newLiveSettings(bc)
	if err != nil {
		return err
	}
	w.live.Store(s)
	return nil
}

func newLiveSettings(bc config.BriefingConfig) (*liveSettings, error) {
	bc.Channels = append([]string(nil), bc.Channels...)
	s := &liveSettings{cfg: bc}
	if strings.TrimSpace(bc.Schedule) == "" {
		return s, nil
	}
	sched, err := parseCron(bc.Schedule)
	if err != nil {
		return nil, err
	}
	s.schedule = sched
	return s, nil
}

// settings is the worker's current briefing configuration. A worker built
// without New (tests) reads its schedule and cfg fields.
func (w *Worker) settings() *liveSettings {
	if s := w.live.Load(); s != nil {
		return s
	}
	s := &liveSettings{schedule: w.schedule}
	if w.cfg != nil {
		s.cfg = w.cfg.Briefing
	}
	return s
}

// mcpConfig is the process MCP config the stdout channel checks.
func (w *Worker) mcpConfig() config.MCPConfig {
	if w.cfg == nil {
		return config.MCPConfig{}
	}
	return w.cfg.MCP
}
