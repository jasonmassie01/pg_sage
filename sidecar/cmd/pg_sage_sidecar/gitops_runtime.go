package main

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/gitops"
)

// The GitOps directory reconciler (cli-02a): with policy.directory set,
// the directory is reconciled into the control database at start, and
// again whenever its files change (polled) or a reconcile failed, and at
// least every gitopsResyncPolls polls so a lapsed pending change is asked
// again. Only the fleet leader writes; a follower reconciles read only.
// policy.directory and policy.directory_mode take effect at restart.

const (
	gitopsPollInterval = 30 * time.Second
	gitopsResyncPolls  = 20
)

// gitopsStatus is the last reconcile, for GET /api/v1/gitops.
var gitopsStatus gitops.Status

// gitopsDirectory is policy.directory, relative paths resolved against
// the config file's directory ("" when off).
func gitopsDirectory(c *config.Config) string {
	if c == nil {
		return ""
	}
	dir := strings.TrimSpace(c.Policy.Directory)
	if dir == "" || filepath.IsAbs(dir) || c.ConfigPath == "" {
		return dir
	}
	return filepath.Join(filepath.Dir(c.ConfigPath), dir)
}

// gitopsLoop is one reconciler's state between polls.
type gitopsLoop struct {
	dir       string
	mode      string
	pool      *pgxpool.Pool
	databases func() []string
	leader    func() bool
	lastHash  string
	lastLog   string
	failed    bool
	polls     int
}

// startGitopsReconciler reconciles the directory once now, then polls it
// until ctx ends.
func startGitopsReconciler(ctx context.Context, control *pgxpool.Pool,
	mgr *fleet.DatabaseManager) {
	dir := gitopsDirectory(cfg)
	if dir == "" || control == nil {
		return
	}
	api.SetGitopsReport(gitopsStatus.Report)
	g := &gitopsLoop{dir: dir, mode: cfg.Policy.EffectiveDirectoryMode(), pool: control,
		databases: func() []string { return monitoredNames(mgr) },
		leader:    func() bool { return fleetLeader == nil || fleetLeader.IsLeader() }}
	logInfo("gitops", "policy.directory %s in %s mode", dir, g.mode)
	g.tick(ctx)
	go func() {
		t := time.NewTicker(gitopsPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				g.tick(ctx)
			}
		}
	}()
}

func monitoredNames(mgr *fleet.DatabaseManager) []string {
	if mgr == nil {
		return nil
	}
	out := make([]string, 0)
	for name := range mgr.Instances() {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// tick loads the directory and reconciles it when it changed, the last
// run failed, or a resync is due.
func (g *gitopsLoop) tick(ctx context.Context) {
	g.polls++
	dir, err := gitops.Load(g.dir)
	if err != nil {
		g.fail(gitops.Result{Mode: g.mode}, err)
		return
	}
	if dir.Hash == g.lastHash && !g.failed && g.polls%gitopsResyncPolls != 0 {
		return
	}
	res, err := gitops.Reconcile(ctx, g.pool, dir, gitops.Options{Mode: g.mode,
		Databases: g.databases(), Leader: g.leader()})
	if err != nil {
		g.fail(res, err)
		return
	}
	g.failed, g.lastHash = false, dir.Hash
	gitopsStatus.Record(g.dir, res, nil, time.Now())
	g.logResult(res)
}

// fail records err; it is logged once until it changes.
func (g *gitopsLoop) fail(res gitops.Result, err error) {
	g.failed = true
	gitopsStatus.Record(g.dir, res, err, time.Now())
	if msg := err.Error(); msg != g.lastLog {
		g.lastLog = msg
		if errors.Is(err, gitops.ErrDirectoryMissing) {
			logWarn("gitops", "%v; nothing is reconciled until it exists", err)
			return
		}
		logError("gitops", "reconcile failed, the previous state stands: %v", err)
	}
}

// logResult logs a diff once (a dry run repeats it only when it changes).
func (g *gitopsLoop) logResult(res gitops.Result) {
	if len(res.Changes) == 0 {
		return
	}
	lines := make([]string, 0, len(res.Changes))
	for _, c := range res.Changes {
		lines = append(lines, c.Op+" "+c.Kind+" "+c.Name)
	}
	msg := strings.Join(lines, "; ")
	if !res.Applied && msg == g.lastLog {
		return
	}
	g.lastLog = msg
	switch {
	case res.Applied:
		logInfo("gitops", "reconciled %s (%s): %s", g.dir, res.Hash, msg)
	case res.Follower:
		logInfo("gitops", "follower: the leader reconciles; pending diff: %s", msg)
	default:
		logInfo("gitops", "%s mode, nothing written; diff: %s", res.Mode, msg)
	}
}
