package clone

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// CommandAdapter is the receipt adapter name of a self-managed restore
// command (drill-01c): an operator program that restores the database's
// own backup chain (pgBackRest, Barman, WAL-G) somewhere the sidecar can
// connect, so a drill proves the backups, not a DBLab copy.
const CommandAdapter = "restore_command"

// Output bounds: more stdout is a failed create, stderr keeps its tail.
const (
	CommandMaxStdout = 64 << 10
	CommandMaxStderr = 16 << 10
)

// DefaultCommandDestroyTimeout bounds one run of the destroy program.
const DefaultCommandDestroyTimeout = 10 * time.Minute

// Restore command errors. ErrCommandNotConfigured is no provider at all
// (the drill reports recoverability unknown), never a failed drill.
var (
	ErrCommandNotConfigured  = errors.New("restore command: no restore_argv is set")
	ErrCommandEmptyProgram   = errors.New("restore command: the program (argv[0]) is empty")
	ErrCommandInvalidConfig  = errors.New("restore command: invalid configuration")
	ErrCommandNoDrillID      = errors.New("restore command: no receipted drill id")
	ErrCommandFailed         = errors.New("restore command failed")
	ErrCommandBadOutput      = errors.New("restore command: stdout is not one JSON line")
	ErrCommandMissingField   = errors.New("restore command: the JSON line lacks a field")
	ErrCommandDSNIsSource    = errors.New("restore command: the printed dsn is the source")
	ErrCommandOutputTooLarge = errors.New("restore command: stdout exceeds 64 KiB")
	ErrCommandOutputHeld     = errors.New("restore command: a process it started kept " +
		"its output open")
	ErrCommandNoUpgrade = errors.New("restore command: no upgrade_argv is set, so it " +
		"cannot upgrade a rehearsal clone")
)

// CommandConfig is one restore command. Programs run without a shell, in
// WorkDir, with a minimal environment: PATH (and SystemRoot on Windows),
// SAGE_DRILL_ID and SAGE_DRILL_DATABASE (restore) or SAGE_CLONE_ID
// (destroy), and the EnvAllow variables that are set.
type CommandConfig struct {
	// RestoreArgv prints {"dsn","id","recovery_point"} on one line.
	RestoreArgv []string
	// DestroyArgv removes the restore named by SAGE_CLONE_ID.
	DestroyArgv []string
	// UpgradeArgv (drill-02b, optional) upgrades the rehearsal clone named
	// by SAGE_CLONE_ID to SAGE_TARGET_MAJOR; it prints nothing (upgraded
	// in place) or one JSON line {"dsn"} naming where the clone now runs.
	UpgradeArgv []string
	EnvAllow    []string
	WorkDir     string
	// MaxDuration bounds a restore (the drill's max_duration_minutes).
	MaxDuration time.Duration
	// DestroyTimeout bounds a destroy; 0 is DefaultCommandDestroyTimeout.
	DestroyTimeout time.Duration
	// SourceDSN is the monitored database: a restore that prints it is
	// refused.
	SourceDSN string
	// Database is the drilled database's pg_sage name, passed to the
	// restore program as SAGE_DRILL_DATABASE (a fleet's wrapper picks the
	// backup by it); empty for the reaper's destroy-only use.
	Database string
	// LookupEnv reads the sidecar's environment; nil is os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Configured reports whether a restore program is set.
func (c CommandConfig) Configured() bool { return len(c.RestoreArgv) > 0 }

// CommandProvider is a clone.Provider over the operator's restore and
// destroy programs. It describes as a full copy, so clone.allow_full_copy
// gates it; it creates only under a receipt (CloneSpec.ResourceName is
// the SAGE_DRILL_ID).
type CommandProvider struct {
	cfg CommandConfig
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// NewCommandProvider checks cfg and returns its provider.
func NewCommandProvider(cfg CommandConfig) (*CommandProvider, error) {
	if !cfg.Configured() {
		return nil, ErrCommandNotConfigured
	}
	if err := validateCommandArgv("restore_argv", cfg.RestoreArgv); err != nil {
		return nil, err
	}
	if err := validateCommandArgv("destroy_argv", cfg.DestroyArgv); err != nil {
		return nil, err
	}
	if len(cfg.UpgradeArgv) > 0 {
		if err := validateCommandArgv("upgrade_argv", cfg.UpgradeArgv); err != nil {
			return nil, err
		}
	}
	if cfg.MaxDuration <= 0 {
		return nil, fmt.Errorf("%w: max duration must be positive", ErrCommandInvalidConfig)
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		return nil, fmt.Errorf("%w: a work directory is required", ErrCommandInvalidConfig)
	}
	for _, name := range cfg.EnvAllow {
		if !envName.MatchString(name) || strings.HasPrefix(strings.ToUpper(name), "SAGE_") {
			return nil, fmt.Errorf("%w: env_allow %q must be a variable name outside SAGE_",
				ErrCommandInvalidConfig, name)
		}
	}
	if cfg.DestroyTimeout <= 0 {
		cfg.DestroyTimeout = DefaultCommandDestroyTimeout
	}
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = os.LookupEnv
	}
	return &CommandProvider{cfg: cfg}, nil
}

func validateCommandArgv(key string, argv []string) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return fmt.Errorf("%w: %s", ErrCommandEmptyProgram, key)
	}
	return nil
}

// Describe: a restore from the operator's backups is a whole copy.
func (p *CommandProvider) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindFullCopy, TypicalReady: SnapshotTypicalReady}, nil
}

// Create runs the restore program for the receipt's drill id and returns
// the clone its one JSON line names.
func (p *CommandProvider) Create(ctx context.Context, spec CloneSpec) (Clone, error) {
	if strings.TrimSpace(spec.ResourceName) == "" {
		return Clone{}, ErrCommandNoDrillID
	}
	out, err := p.run(ctx, p.cfg.RestoreArgv, p.cfg.MaxDuration, "max_duration_minutes",
		"SAGE_DRILL_ID", spec.ResourceName)
	if err != nil {
		return Clone{}, err
	}
	cl, err := parseCommandLine(out, p.cfg.SourceDSN, time.Now())
	if err != nil {
		// The program exited 0 saying it restored something: whatever it
		// started may be running, so the receipt stays create_uncertain.
		return Clone{}, fmt.Errorf("%w: %w", ErrCloneLeftBehind, err)
	}
	return cl, nil
}

// Destroy runs the destroy program for the clone's id (SAGE_CLONE_ID).
func (p *CommandProvider) Destroy(ctx context.Context, target Clone) error {
	if err := validCommandCloneID(target.ID); err != nil {
		return err
	}
	_, err := p.run(ctx, p.cfg.DestroyArgv, p.cfg.DestroyTimeout, "the destroy timeout",
		"SAGE_CLONE_ID", target.ID)
	return err
}

// SnapshotAge is unknown: the backup chain is read only by a restore.
func (p *CommandProvider) SnapshotAge(context.Context) (time.Duration, error) {
	return 0, errors.New("restore command: backup age is unknown until a restore runs")
}

// validCommandCloneID checks an id the restore program printed: it
// becomes a receipt ref and the destroy program's SAGE_CLONE_ID.
func validCommandCloneID(id string) error {
	if strings.TrimSpace(id) == "" || len(id) > maxReceiptText {
		return fmt.Errorf("%w: id must be 1-%d characters", ErrCommandMissingField,
			maxReceiptText)
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: id holds a control character", ErrCommandBadOutput)
		}
	}
	return nil
}

// CommandScope is the receipt scope of a restore command: its program,
// hashed when too long for a receipt.
func CommandScope(restoreArgv []string) string {
	program := ""
	if len(restoreArgv) > 0 {
		program = restoreArgv[0]
	}
	scope := "command/" + program
	if len(scope) <= maxReceiptText {
		return scope
	}
	sum := sha256.Sum256([]byte(program))
	return scope[:maxReceiptText-17] + "#" + hex.EncodeToString(sum[:8])
}

var (
	_ Provider  = (*CommandProvider)(nil)
	_ Describer = (*CommandProvider)(nil)
)
