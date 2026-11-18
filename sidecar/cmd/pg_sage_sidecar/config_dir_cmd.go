package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/pg-sage/sidecar/internal/gitops"
)

// `config validate <dir>` and `config diff <dir>` (cli-02a): a GitOps
// directory (policy.directory) instead of a config file. validate loads it
// offline. diff reads the running sidecar's last reconcile of its own
// policy.directory and shows its diff when the sidecar saw the same files
// (same hash); otherwise it says the sidecar has not reconciled them yet.

// isDirectory reports whether path names a directory.
func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// validateDirectory: exit 0 valid, 2 invalid (every problem with its file
// and line), 1 unreadable.
func validateDirectory(path string, env cliEnv) int {
	dir, code := loadDirectory("config validate", path, env)
	if dir == nil {
		return code
	}
	_, _ = fmt.Fprintf(env.stdout, "%s: valid; %d policy set(s), %d principal(s), "+
		"%d grant(s); hash %s\n", path, len(dir.Sets), len(dir.Principals), len(dir.Grants),
		dir.Hash)
	return configExitOK
}

func loadDirectory(verb, path string, env cliEnv) (*gitops.Directory, int) {
	dir, err := gitops.Load(path)
	var invalid *gitops.InvalidError
	switch {
	case errors.As(err, &invalid):
		noun := "problems"
		if len(invalid.Problems) == 1 {
			noun = "problem"
		}
		_, _ = fmt.Fprintf(env.stdout, "%s: invalid, %d %s:\n", path, len(invalid.Problems),
			noun)
		for _, p := range invalid.Problems {
			_, _ = fmt.Fprintf(env.stdout, "  %s\n", p)
		}
		return nil, configExitProblem
	case err != nil:
		_, _ = fmt.Fprintf(env.stderr, "%s: %v\n", verb, err)
		return nil, configExitError
	}
	return dir, configExitOK
}

// gitopsAnswer is GET /api/v1/gitops.
type gitopsAnswer struct {
	Configured bool          `json:"configured"`
	Report     gitops.Report `json:"report"`
}

// diffDirectory: exit 0 when the sidecar reconciled these files and has
// nothing left to do, 2 when anything differs or waits, 1 on an error.
func diffDirectory(ctx context.Context, path string, f cliFlags, env cliEnv) int {
	dir, code := loadDirectory("config diff", path, env)
	if dir == nil {
		if code == configExitProblem {
			_, _ = fmt.Fprintln(env.stderr, "config diff: fix the files first (config validate)")
			return configExitError
		}
		return code
	}
	s, code := connect("config diff", f, env, configExitAuth)
	if s == nil {
		return code
	}
	ans, err := s.api.send(ctx, http.MethodGet, "/api/v1/gitops", nil, nil)
	if err != nil {
		return s.fail(err, configExitAuth)
	}
	var got gitopsAnswer
	raw, err := json.Marshal(ans.body)
	if err == nil {
		err = json.Unmarshal(raw, &got)
	}
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "config diff: the sidecar's answer is unreadable: %v\n",
			err)
		return configExitError
	}
	return printDirectoryDiff(env, path, dir, got)
}

func printDirectoryDiff(env cliEnv, path string, dir *gitops.Directory, got gitopsAnswer) int {
	r := got.Report
	switch {
	case !got.Configured:
		_, _ = fmt.Fprintln(env.stdout, "the sidecar has no policy.directory; set it and "+
			"restart to reconcile these files")
		return configExitProblem
	case r.Error != "":
		_, _ = fmt.Fprintf(env.stdout, "the sidecar's last reconcile of %s failed: %s\n",
			r.Directory, r.Error)
		return configExitProblem
	case r.Hash != dir.Hash:
		_, _ = fmt.Fprintf(env.stdout, "the sidecar has not reconciled these files: it last "+
			"saw %s (hash %s), %s is hash %s\n", r.Directory, r.Hash, path, dir.Hash)
		return configExitProblem
	case len(r.Changes) == 0:
		_, _ = fmt.Fprintf(env.stdout, "no differences: the sidecar's rows match %s\n", path)
		return configExitOK
	}
	_, _ = fmt.Fprintf(env.stdout, "%s mode; applied: %v\n", r.Mode, r.Applied)
	for _, c := range r.Changes {
		_, _ = fmt.Fprintf(env.stdout, "  %s %s %s  %s %s\n", c.Op, c.Kind, c.Name, c.File,
			c.Detail)
	}
	_, _ = fmt.Fprintf(env.stdout, "%d change(s) for %s\n", len(r.Changes), path)
	return configExitProblem
}
