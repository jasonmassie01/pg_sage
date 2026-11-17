package onboarding

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The setup wizard's steps whose first-done time sage.onboarding keeps
// (ux-04b). The connection is the checklist's own Connected step.
const (
	SetupModel    = "model"
	SetupChannel  = "channel"
	SetupAutonomy = "autonomy"
)

// ErrUnknownSetupStep is a setup step other than model, channel or autonomy.
var ErrUnknownSetupStep = errors.New("onboarding: unknown setup step")

// setupColumns maps each step to its column; the SQL below only ever names
// one of these constants, never caller text.
var setupColumns = map[string]string{
	SetupModel:    "model_configured_at",
	SetupChannel:  "channel_configured_at",
	SetupAutonomy: "autonomy_chosen_at",
}

// RecordSetupStep stores when step was first finished for database; a
// later call leaves the first time. It reports whether it stored one, and
// updates the in-process within-the-hour gauge from the stored row.
func RecordSetupStep(ctx context.Context, pool *pgxpool.Pool, database, step string,
	at time.Time) (bool, error) {
	column, ok := setupColumns[step]
	if !ok {
		return false, fmt.Errorf("%w %q (want model, channel or autonomy)",
			ErrUnknownSetupStep, step)
	}
	changed, err := recordOnce(ctx, pool, database, `UPDATE sage.onboarding SET `+column+
		` = $2 WHERE database_name = $1 AND `+column+` IS NULL RETURNING 1`, at)
	if err != nil {
		return false, err
	}
	st, found, err := Get(ctx, pool, database)
	if err != nil {
		return changed, err
	}
	if found {
		ObserveSetup(st)
	}
	return changed, nil
}

// setupWindow is how soon after installed_at a step counts as set up
// "within the hour".
const setupWindow = time.Hour

// WithinHour reports whether at is at most one hour after installed: the
// boundary itself (3,600 s) counts. A step never finished (nil) does not.
func WithinHour(installed time.Time, at *time.Time) bool {
	return at != nil && at.Sub(installed) <= setupWindow
}

type setupMark struct{ model, channel bool }

// setupGauge holds, per new install this process monitors, whether its
// model and channel were set within the hour.
type setupGauge struct {
	mu  sync.Mutex
	dbs map[string]setupMark
}

func newSetupGauge() *setupGauge { return &setupGauge{dbs: map[string]setupMark{}} }

var setups = newSetupGauge()

// ObserveSetup records a database's state in the gauge. Only new installs
// count: an existing install was set up before pg_sage measured it.
func ObserveSetup(st State) {
	if st.Database == "" || st.InstallKind != InstallNew {
		return
	}
	g := setups
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dbs[st.Database] = setupMark{model: WithinHour(st.InstalledAt, st.ModelConfiguredAt),
		channel: WithinHour(st.InstalledAt, st.ChannelConfiguredAt)}
}

// ForgetSetup drops a database the process no longer monitors.
func ForgetSetup(database string) {
	g := setups
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.dbs, database)
}

// SetupRatio is pg_sage_setup_within_hour_ratio: of Installs new installs,
// the share with a model (Model) and a channel (Channel) within the hour.
type SetupRatio struct {
	Installs       int
	Model, Channel float64
}

// SetupRatios reads the gauge; Installs is 0 when no new install is known.
func SetupRatios() SetupRatio {
	g := setups
	g.mu.Lock()
	defer g.mu.Unlock()
	r := SetupRatio{Installs: len(g.dbs)}
	if r.Installs == 0 {
		return r
	}
	var model, channel int
	for _, m := range g.dbs {
		if m.model {
			model++
		}
		if m.channel {
			channel++
		}
	}
	r.Model = float64(model) / float64(r.Installs)
	r.Channel = float64(channel) / float64(r.Installs)
	return r
}
