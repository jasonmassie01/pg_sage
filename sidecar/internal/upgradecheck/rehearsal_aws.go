package upgradecheck

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Defaults of an AWS clone upgrade's wait.
const (
	DefaultAWSUpgradePoll    = 30 * time.Second
	DefaultAWSUpgradeMaxWait = 2 * time.Hour
)

// AWSUpgrader upgrades an Aurora clone cluster or an RDS clone instance
// (the aurora_clone and rds_restore adapters' clones) to the newest engine
// version AWS offers at the target major. It acts only on a resource that
// a live rehearsal receipt names and that is not SourceID, and it fails
// closed when either cannot be shown.
type AWSUpgrader struct {
	// Kind names the resource in messages ("aurora" or "rds").
	Kind string
	API  clone.EngineUpgradeAPI
	// SourceID is the monitored cluster or instance identifier.
	SourceID string
	// Live reports whether id names a ready, unexpired rehearsal receipt
	// (LiveRehearsalReceipt).
	Live      func(ctx context.Context, id string) (bool, error)
	PollEvery time.Duration
	MaxWait   time.Duration
}

// LiveRehearsalReceipt is AWSUpgrader.Live over the receipt store: a
// ready, unexpired receipt of purpose rehearsal records the identifier as
// its clone's resource id (the receipt's ref; its name is another string,
// rehearsal-<uuid>, from which the adapters derive sage-rehearsal-<hex>).
func LiveRehearsalReceipt(store *clone.ReceiptStore) func(context.Context, string) (bool,
	error) {
	return func(ctx context.Context, id string) (bool, error) {
		if store == nil {
			return false, clone.ErrNoReceiptStore
		}
		r, ok, err := store.ActiveRef(ctx, id)
		if err != nil || !ok {
			return false, err
		}
		return r.Purpose == clone.PurposeRehearsal, nil
	}
}

// Upgrade moves cl to targetMajor in place and waits until it is
// available at that major; the endpoint (cl's DSN) does not change.
func (u AWSUpgrader) Upgrade(ctx context.Context, cl clone.Clone, targetMajor int) (clone.Clone,
	error) {
	if err := u.refuse(ctx, cl.ID); err != nil {
		return clone.Clone{}, err
	}
	state, err := u.API.Engine(ctx, cl.ID)
	if err != nil {
		return clone.Clone{}, fmt.Errorf("%s clone %s: read the engine: %w", u.Kind, cl.ID, err)
	}
	if current := engineMajor(state.Version); targetMajor <= current || targetMajor > 1000 {
		return clone.Clone{}, fmt.Errorf("%w: %s clone %s runs %s; target major %d is not "+
			"an upgrade", ErrInvalidRehearsal, u.Kind, cl.ID, state.Version, targetMajor)
	}
	targets, err := u.API.UpgradeTargets(ctx, state.Engine, state.Version)
	if err != nil {
		return clone.Clone{}, fmt.Errorf("%s clone %s: list upgrade targets of %s %s: %w",
			u.Kind, cl.ID, state.Engine, state.Version, err)
	}
	version, ok := newestOfMajor(targets, targetMajor)
	if !ok {
		return clone.Clone{}, fmt.Errorf("%w: AWS offers no upgrade of %s %s to major %d "+
			"(targets %v)", ErrUpgradeUnsupported, state.Engine, state.Version, targetMajor,
			targets)
	}
	if err := u.API.ModifyEngineVersion(ctx, cl.ID, version); err != nil {
		return clone.Clone{}, fmt.Errorf("%s clone %s: upgrade to %s: %w", u.Kind, cl.ID,
			version, err)
	}
	if err := u.wait(ctx, cl.ID, targetMajor); err != nil {
		return clone.Clone{}, err
	}
	return cl, nil
}

func (u AWSUpgrader) refuse(ctx context.Context, id string) error {
	switch {
	case strings.TrimSpace(id) == "":
		return fmt.Errorf("%w: the clone has no resource id", ErrUpgradeRefused)
	case u.SourceID == "":
		return fmt.Errorf("%w: the source identifier is unknown, so %s cannot be shown "+
			"not to be the source", ErrUpgradeRefused, id)
	case strings.EqualFold(id, u.SourceID):
		return fmt.Errorf("%w: %s is the source %s; only a rehearsal clone is upgraded",
			ErrUpgradeRefused, id, u.Kind)
	case u.Live == nil:
		return fmt.Errorf("%w: no receipt check, so %s cannot be shown to be a rehearsal "+
			"clone", ErrUpgradeRefused, id)
	}
	live, err := u.Live(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: read the rehearsal receipt of %s: %w", ErrUpgradeRefused, id,
			err)
	}
	if !live {
		return fmt.Errorf("%w: no live rehearsal receipt names %s", ErrUpgradeRefused, id)
	}
	return nil
}

// wait polls until the resource is available at major.
func (u AWSUpgrader) wait(ctx context.Context, id string, major int) error {
	poll, limit := u.PollEvery, u.MaxWait
	if poll <= 0 {
		poll = DefaultAWSUpgradePoll
	}
	if limit <= 0 {
		limit = DefaultAWSUpgradeMaxWait
	}
	deadline := time.Now().Add(limit)
	for {
		s, err := u.API.Engine(ctx, id)
		if err != nil {
			return fmt.Errorf("%s clone %s: read the upgrade's progress: %w", u.Kind, id, err)
		}
		if s.Status == "available" && engineMajor(s.Version) == major {
			return nil
		}
		if failedEngineStatus[s.Status] {
			return fmt.Errorf("%s clone %s: the upgrade stopped in status %s at %s", u.Kind,
				id, s.Status, s.Version)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s clone %s: the upgrade did not finish within %s (status %s "+
				"at %s)", u.Kind, id, limit, s.Status, s.Version)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s clone %s: waiting for the upgrade: %w", u.Kind, id,
				ctx.Err())
		case <-time.After(poll):
		}
	}
}

// failedEngineStatus are statuses an upgrade does not leave by itself.
var failedEngineStatus = map[string]bool{"failed": true, "incompatible-parameters": true,
	"incompatible-restore": true, "inaccessible-encryption-credentials": true,
	"storage-full": true}

// engineMajor is the major of an engine version such as "17.4" (0 when
// it is not one).
func engineMajor(version string) int {
	head, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(head)
	if err != nil {
		return 0
	}
	return n
}

// newestOfMajor is the highest version of major among versions, by
// numeric minor ("17.10" above "17.4").
func newestOfMajor(versions []string, major int) (string, bool) {
	best, bestMinor := "", -1
	for _, v := range versions {
		if engineMajor(v) != major {
			continue
		}
		_, rest, _ := strings.Cut(v, ".")
		head, _, _ := strings.Cut(rest, ".")
		minor, err := strconv.Atoi(head)
		if err != nil {
			minor = 0
		}
		if minor > bestMinor {
			best, bestMinor = v, minor
		}
	}
	return best, best != ""
}

// NeonUpgrader is Neon's answer: a branch inherits its project's
// PostgreSQL major, and a project's major is fixed when it is created (the
// Neon API has no operation that changes it), so a Neon clone cannot be
// upgraded. Rehearse a Neon upgrade by restoring into a new project at the
// target major through a command (drill.command).
type NeonUpgrader struct{}

// Upgrade always refuses with ErrUpgradeUnsupported.
func (NeonUpgrader) Upgrade(context.Context, clone.Clone, int) (clone.Clone, error) {
	return clone.Clone{}, fmt.Errorf("%w: Neon cannot change a branch's or a project's "+
		"PostgreSQL major; a project's major is set when it is created", ErrUpgradeUnsupported)
}
