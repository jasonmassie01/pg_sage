// Package connections is Sage Review's connection-architecture generator
// (review-02e). From the window's workload_sessions snapshots it finds:
//
//   - recycle herds: more than HerdShare of an application's sessions
//     connected in the same minute, in at least HerdMinSamples samples, so
//     they reach their pool's lifetime together and reconnect at once;
//   - each application's mix of active, idle and idle in transaction
//     sessions (Mixes), and pooler sizing for mostly idle ones;
//   - roles idle in a transaction longer than IdleInTxLimit;
//   - the headroom at peak against max_connections less the reserved slots,
//     and the roles over their fair share of it.
//
// Steps use the role-budget executor contracts: set_role_timeout for an
// idle_in_transaction_session_timeout and set_role_connection_limit for a
// limit, which is never below the role's observed peak. Both always need
// approval here: a review has no budget's warn-mode shadow data. Pooler
// mode, pool size and lifetimes are advice only; pg_sage never changes a
// pooler (spec section 3). Nothing here executes or writes.
//
// Application names stay on the local side: the pack's connections
// section holds none, and candidates are not part of any model tier.
package connections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/review/candidate"
)

// Name is the generator's name; Dimension the rubric dimension it serves
// (configuration.connection_tuning).
const (
	Name      = "connections"
	Dimension = "configuration"
)

// Candidate kinds.
const (
	KindRecycleHerd     = "recycle_herd"
	KindPoolerSizing    = "pooler_sizing"
	KindIdleInTxTimeout = "idle_in_transaction_timeout"
	KindConnectionLimit = "role_connection_limit"
)

// The executor contracts the steps use.
const (
	ActionRoleTimeout     = executor.ActionTypeSetRoleTimeout
	ActionConnectionLimit = executor.ActionTypeSetRoleConnectionLimit
)

// Prediction quantities.
const (
	QuantityHerd           = "herd"
	QuantityHerdSamples    = "herd_samples"
	QuantityHerdSessions   = "herd_sessions"
	QuantityHerdSharePct   = "herd_share_pct"
	QuantityPoolSize       = "pool_size"
	QuantityIdleSharePct   = "idle_share_pct"
	QuantityIdleAge        = "idle_in_transaction_seconds"
	QuantityTimeoutSeconds = "timeout_seconds"
	QuantityLimit          = "connection_limit"
	QuantityRolePeak       = "role_peak_sessions"
	QuantityFairShare      = "fair_share"
	QuantityHeadroomPct    = "headroom_pct"
)

// Generator is the connection generator. Its source reads the sessions;
// a nil source reads nothing.
type Generator struct{ src Source }

// New is a generator reading through src (nil: nothing).
func New(src Source) *Generator { return &Generator{src: src} }

// Name is "connections".
func (g *Generator) Name() string { return Name }

var _ candidate.Generator = (*Generator)(nil)

// Generate reads the pack window's sessions and builds the candidates
// (Build). A source error and a canceled context are errors.
func (g *Generator) Generate(ctx context.Context, pack *workpack.Pack, env candidate.Env,
) ([]candidate.Candidate, candidate.Coverage, error) {
	if pack == nil {
		return nil, candidate.Coverage{}, errors.New("connections generator: no evidence pack")
	}
	if err := ctx.Err(); err != nil {
		return nil, candidate.Coverage{}, fmt.Errorf("connections generator for %s: %w",
			env.Database, err)
	}
	var in Inputs
	if g.src != nil {
		var err error
		if in, err = g.src.Read(ctx, pack.Window.From, pack.Window.To); err != nil {
			return nil, candidate.Coverage{}, fmt.Errorf(
				"connections generator for %s: read the workload sessions: %w",
				env.Database, err)
		}
	}
	return Build(pack, in, env)
}

// Build judges in against the pack. A pack without its connections
// section, or inputs with no sessions, give no candidates and
// Coverage.Missing ["connections"]. Every candidate is validated.
func Build(pack *workpack.Pack, in Inputs, env candidate.Env) ([]candidate.Candidate,
	candidate.Coverage, error) {
	if err := checkInput(pack, env); err != nil {
		return nil, candidate.Coverage{}, err
	}
	cov := candidate.WindowCoverage(pack, env.WindowDays)
	v, err := readView(pack)
	if err != nil {
		return nil, cov, fmt.Errorf("connections generator for %s: %w", env.Database, err)
	}
	if !v.hasHours || in.empty() {
		cov.AddMissing("connections")
		return nil, cov, nil
	}
	if in.WindowSamples > len(in.Samples) {
		cov.Note("herds were judged on the newest %d of the window's %d session samples",
			len(in.Samples), in.WindowSamples)
	}
	b := &builder{v: v, in: in, cov: &cov}
	b.herds()
	b.poolers()
	b.timeouts()
	b.limits()
	if err := candidate.ValidateAll(pack, b.out); err != nil {
		return nil, cov, fmt.Errorf("connections generator for %s: %w", env.Database, err)
	}
	return b.out, cov, nil
}

func checkInput(pack *workpack.Pack, env candidate.Env) error {
	switch {
	case pack == nil:
		return errors.New("connections generator: no evidence pack")
	case env.Database == "":
		return errors.New("connections generator: no database named")
	case env.WindowDays < 1:
		return fmt.Errorf("connections generator for %s: window of %d days", env.Database,
			env.WindowDays)
	}
	return nil
}

// Herds is the review's number: the recycle herds found.
func Herds(cands []candidate.Candidate) int {
	n := 0
	for _, c := range cands {
		if c.Generator == Name && c.Prediction.Quantities[QuantityHerd] == 1 {
			n++
		}
	}
	return n
}

// HeadroomPct is the review's other number: the free share of usable
// connections (max_connections less the reserved slots) at the window's
// peak, in percent. ok is false when the pack does not measure both. The
// headroom after accepted steps is the next review's.
func HeadroomPct(pack *workpack.Pack) (float64, bool) {
	v, err := readView(pack)
	if err != nil || !v.measured {
		return 0, false
	}
	return v.headroomPct(), true
}

// readSettings decodes the settings section (ok false without one).
func readSettings(pack *workpack.Pack) (workpack.Settings, bool, error) {
	raw := pack.File("settings.json")
	if raw == nil {
		return workpack.Settings{}, false, nil
	}
	var s workpack.Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return workpack.Settings{}, false, fmt.Errorf("decode settings.json: %w", err)
	}
	return s, true, nil
}

// hourOfWeek is t's hour of week (Monday 00:00 is 0) in loc, as the
// pack's connections section buckets it.
func hourOfWeek(t time.Time, loc *time.Location) int {
	t = t.In(loc)
	return (int(t.Weekday())+6)%7*24 + t.Hour()
}
