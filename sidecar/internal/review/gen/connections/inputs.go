package connections

import (
	"context"
	"time"

	"github.com/pg-sage/sidecar/internal/budget"
	"github.com/pg-sage/sidecar/internal/collector"
)

// Sample is one workload_sessions snapshot as the herd rule reads it.
// Apps is each application's client sessions (its groups summed); Ages
// its sessions by connection minute, nil on a snapshot written before
// they were collected; AgesTruncated says the collector's cap cut them.
type Sample struct {
	At            time.Time
	Apps          map[string]int
	Ages          []collector.WorkloadAppAge
	AgesTruncated bool
}

// RolePeak is a role's most cluster-wide client sessions in one snapshot
// of the window, and when.
type RolePeak struct {
	Role     string
	Sessions int
	At       time.Time
}

// IdleInTx is a role's longest session idle in a transaction in the
// window (state age, seconds), and when it was seen.
type IdleInTx struct {
	Role       string
	AgeSeconds float64
	At         time.Time
}

// AppState is one application's sessions in one state over the window:
// the mean per snapshot, the most in one snapshot and when.
type AppState struct {
	Application string
	State       string
	Mean        float64
	Peak        int
	PeakAt      time.Time
}

// Inputs is what the generator reads besides the pack. Samples are the
// window's newest MaxSamples snapshots, oldest first (WindowSamples is how
// many the window holds); RolePeaks, IdleInTx and Mix span the whole
// window. Database is current_database() and SelfRole pg_sage's role on
// the monitored database; Roles their facts, nil when they were not read.
type Inputs struct {
	Samples       []Sample
	WindowSamples int
	RolePeaks     []RolePeak
	IdleInTx      []IdleInTx
	Mix           []AppState
	Database      string
	SelfRole      string
	Roles         map[string]budget.RoleFact
}

// empty reports inputs with no session evidence at all.
func (in Inputs) empty() bool {
	return len(in.Samples) == 0 && len(in.RolePeaks) == 0 && len(in.IdleInTx) == 0 &&
		len(in.Mix) == 0
}

// Source reads the inputs over a window [from, to). Read must be safe for
// concurrent use: one generator serves many databases.
type Source interface {
	Read(ctx context.Context, from, to time.Time) (Inputs, error)
}
