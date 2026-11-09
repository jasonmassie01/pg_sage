package gameday

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pg-sage/sidecar/internal/clone"
)

// Shared-target leases (benches-01). Fault programs act on a whole
// cluster and the safety grader reads the whole database, so two runs on
// one shared target (every runtime of a fleet is given the same local
// game-day database) would grade each other's objects as forbidden
// actions. A run holds its target from Create to Destroy: in this
// process through Leases, across processes through the bench_target
// cluster lock. A clone provider gives each run its own target and is
// never leased.

// BenchTargetLock is the cluster lock a run holds on a shared target. It
// is session-scoped: a crashed holder's lease ends with its connection.
const BenchTargetLock = "bench_target"

// Holder names a run that holds a target.
type Holder struct {
	Kind     string // "game day" or "local bench"
	ID       string
	Database string
}

func (h Holder) String() string { return fmt.Sprintf("%s %s for %s", h.Kind, h.ID, h.Database) }

// Holder kinds.
const (
	kindGameDay    = "game day"
	kindLocalBench = "local bench"
)

// targetLocker is a shared target that can be locked across processes.
type targetLocker interface {
	lockTarget(ctx context.Context) (release func(), ok bool, err error)
}

// Leases are the shared targets this process's runs hold.
type Leases struct {
	mu   sync.Mutex
	held map[string]*lease
}

type lease struct{ holder Holder }

// processLeases is every runtime's: fleet runtimes share one target.
var processLeases = NewLeases()

// NewLeases is an empty lease table.
func NewLeases() *Leases { return &Leases{held: map[string]*lease{}} }

// Acquire leases p's target to h until release is called. A target
// another run holds is ErrRunning naming the holder; a target lock that
// cannot be taken refuses the run (fail closed).
func (l *Leases) Acquire(ctx context.Context, p clone.Provider, h Holder) (func(),
	error) {
	if p == nil {
		return nil, errors.New("lease bench target: no provider")
	}
	st, ok := p.(clone.SharedTarget)
	if !ok {
		return func() {}, nil
	}
	key := strings.TrimSpace(st.TargetKey())
	if key == "" {
		return nil, errors.New("lease bench target: the shared target has no target key")
	}
	mine, err := l.hold(key, h)
	if err != nil {
		return nil, err
	}
	unlock := func() {}
	if lk, ok := p.(targetLocker); ok {
		release, got, err := lk.lockTarget(ctx)
		switch {
		case err != nil:
			l.drop(key, mine)
			return nil, fmt.Errorf("bench target lock on %s: %w", key, err)
		case !got:
			l.drop(key, mine)
			return nil, fmt.Errorf("%w: target busy: another session holds the bench "+
				"target lock on %s", ErrRunning, key)
		}
		unlock = release
	}
	var once sync.Once
	return func() { once.Do(func() { unlock(); l.drop(key, mine) }) }, nil
}

func (l *Leases) hold(key string, h Holder) (*lease, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.held[key]; ok {
		return nil, fmt.Errorf("%w: target busy: %s", ErrRunning, cur.holder)
	}
	mine := &lease{holder: h}
	l.held[key] = mine
	return mine, nil
}

// drop ends mine; a newer holder's lease on key is kept.
func (l *Leases) drop(key string, mine *lease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held[key] == mine {
		delete(l.held, key)
	}
}

func leasesOr(l *Leases) *Leases {
	if l == nil {
		return processLeases
	}
	return l
}
