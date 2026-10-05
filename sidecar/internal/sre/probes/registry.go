package probes

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Registry is an immutable set of validated probe specs.
type Registry struct {
	specs map[ID]Spec
	order []ID
}

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

var knownFamilies = map[string]bool{FamilyLocks: true, FamilyConnections: true,
	FamilyReplication: true, FamilyWAL: true, FamilyVacuum: true, FamilyPlans: true,
	FamilyChange: true, FamilyTempFiles: true, FamilyWaits: true,
	FamilySequences: true, FamilyRunway: true, FamilyStats: true}

var extensionPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// NewRegistry validates specs against the hard ceilings and returns an
// immutable registry. Duplicate ids are rejected.
func NewRegistry(specs ...Spec) (*Registry, error) {
	r := &Registry{specs: make(map[ID]Spec, len(specs))}
	for _, s := range specs {
		if err := validateSpec(s); err != nil {
			return nil, fmt.Errorf("probe %q: %w", s.ID, err)
		}
		if _, dup := r.specs[s.ID]; dup {
			return nil, fmt.Errorf("probe %q: duplicate id", s.ID)
		}
		r.specs[s.ID] = s
		r.order = append(r.order, s.ID)
	}
	return r, nil
}

func validateSpec(s Spec) error {
	switch {
	case !idPattern.MatchString(string(s.ID)):
		return fmt.Errorf("invalid id")
	case s.Version == "":
		return fmt.Errorf("empty version")
	case !knownFamilies[s.Family]:
		return fmt.Errorf("unknown family %q", s.Family)
	case s.Args < ArgsNone || s.Args > ArgsSlice:
		return fmt.Errorf("unknown argument kind %d", s.Args)
	case s.StatementTimeout <= 0 || s.StatementTimeout > MaxStatementTimeout:
		return fmt.Errorf("statement timeout %s outside (0, %s]",
			s.StatementTimeout, MaxStatementTimeout)
	case s.BackgroundTimeout != 0 && (s.BackgroundTimeout < s.StatementTimeout ||
		s.BackgroundTimeout > MaxBackgroundStatementTimeout):
		return fmt.Errorf("background timeout %s outside [%s, %s]",
			s.BackgroundTimeout, s.StatementTimeout, MaxBackgroundStatementTimeout)
	case s.LockTimeout <= 0 || s.LockTimeout >= s.StatementTimeout:
		return fmt.Errorf("lock timeout %s must be in (0, statement timeout)",
			s.LockTimeout)
	case s.MaxRows <= 0 || s.MaxRows > MaxRows:
		return fmt.Errorf("max rows %d outside (0, %d]", s.MaxRows, MaxRows)
	case s.MaxBytes <= 0 || s.MaxBytes > MaxBytes:
		return fmt.Errorf("max bytes %d outside (0, %d]", s.MaxBytes, MaxBytes)
	}
	for _, r := range s.Requires {
		if !readRoles[r] {
			return fmt.Errorf("required role %q is not a read-only predefined role", r)
		}
	}
	if err := validateVariants(s.Variants); err != nil {
		return err
	}
	return validateExtension(s)
}

// validateExtension requires ExtSchemaToken in every variant of a spec
// that names an extension, and in none of a spec that does not.
func validateExtension(s Spec) error {
	if s.Extension != "" && !extensionPattern.MatchString(s.Extension) {
		return fmt.Errorf("invalid extension name %q", s.Extension)
	}
	for i, v := range s.Variants {
		if has := strings.Contains(v.SQL, ExtSchemaToken); has != (s.Extension != "") {
			return fmt.Errorf("variant %d: %s must appear exactly when an extension is named",
				i, ExtSchemaToken)
		}
	}
	return nil
}

func validateVariants(vs []Variant) error {
	if len(vs) == 0 {
		return fmt.Errorf("no SQL variants")
	}
	for i, v := range vs {
		if strings.TrimSpace(v.SQL) == "" {
			return fmt.Errorf("variant %d has empty SQL", i)
		}
		if i > 0 && v.MinVersion <= vs[i-1].MinVersion {
			return fmt.Errorf("variants must ascend by MinVersion")
		}
	}
	return nil
}

// Spec returns the spec for id. A nil registry knows no probes.
func (r *Registry) Spec(id ID) (Spec, bool) {
	if r == nil {
		return Spec{}, false
	}
	s, ok := r.specs[id]
	return s, ok
}

// IDs lists the registered probes in registration order.
func (r *Registry) IDs() []ID {
	if r == nil {
		return nil
	}
	return append([]ID(nil), r.order...)
}

// catalog is the validated R1 catalog, built once.
var catalog = mustRegistry(catalogSpecs()...)

func mustRegistry(specs ...Spec) *Registry {
	r, err := NewRegistry(specs...)
	if err != nil {
		panic("sre probe catalog is invalid: " + err.Error())
	}
	return r
}

// Catalog returns the fixed R1 probe catalog.
func Catalog() *Registry { return catalog }

func catalogSpecs() []Spec {
	specs := []Spec{
		lockChainsSpec(), lockGraphSpec(), longTransactionsSpec(),
		preparedXactsSpec(), backendIdentitySpec(), connectionSaturationSpec(),
		replicationLagSpec(), replicationSlotsSpec(), walCheckpointSpec(),
		autovacuumWraparoundSpec(), vacuumProgressSpec(), planRegressionsSpec(),
		archiverSpec(), sageActionsSpec(), checkpointActivitySpec(),
		tempFileActivitySpec(), tempFileHoldersSpec(), tempSpillStatementsSpec(),
		standbyReplayStateSpec(), lwlockWaitsSpec(),
	}
	return append(append(specs, runwaySpecs()...), statViewSpecs()...)
}

// signalProbes maps RCA incident signals to the catalog probes that
// investigate them, most discriminating first.
var signalProbes = map[string][]ID{
	"lock_contention":            {LockGraph, LockChains, LongTransactions, PreparedXacts},
	"idle_in_tx_elevated":        {LongTransactions, LockGraph},
	"orphaned_prepared_tx":       {PreparedXacts, LockGraph},
	"connections_high":           {ConnectionSaturation, LockGraph, LongTransactions},
	"replication_lag_increasing": {ReplicationLag, ReplicationSlots},
	"wal_growth_spike":           {WALCheckpoint, ReplicationSlots},
	"vacuum_blocked":             {AutovacuumWraparound, VacuumProgress, LongTransactions},
	// M6 log signals.
	"log_checkpoint_too_frequent": {CheckpointActivity},
	"log_temp_file_created":       {TempFileActivity, TempFileHolders, TempSpillStatements},
	"log_replication_conflict":    {StandbyReplayState, ReplicationLag},
}

// ForSignal returns the probes that investigate an RCA signal (nil for
// signals without a catalog family).
func ForSignal(signal string) []ID {
	return append([]ID(nil), signalProbes[signal]...)
}

// RoleReadAllStats lets a role see every session's state, wait event,
// transaction start and backend type in pg_stat_activity (and the lag
// columns of pg_stat_replication); pg_monitor includes it.
const RoleReadAllStats = "pg_read_all_stats"

// readRoles are the predefined roles a spec may require: read-only ones.
var readRoles = map[string]bool{RoleReadAllStats: true, "pg_read_all_settings": true,
	"pg_monitor": true}

// needsStats marks a probe whose views hide other roles' sessions
// without pg_read_all_stats.
func needsStats(s Spec) Spec {
	s.Requires = []string{RoleReadAllStats}
	return s
}

// spec builds a catalog spec with the R1 default caps.
func spec(id ID, family string, args ArgKind, variants ...Variant) Spec {
	return Spec{ID: id, Version: "v1", Family: family, Args: args,
		Variants: variants, StatementTimeout: MaxStatementTimeout,
		LockTimeout: 100 * time.Millisecond, MaxRows: MaxRows, MaxBytes: MaxBytes}
}
