package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/gitops"
)

// cli-02a wiring: policy.directory resolution and the poll loop's failure
// handling. The reconcile itself is covered on a real server in
// internal/gitops. No concurrent test: one loop owns its state.

func TestGitopsDirectory_ResolvesAgainstTheConfigFile(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "policy")
	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"nil config", nil, ""},
		{"off", &config.Config{ConfigPath: "/etc/sage/config.yaml"}, ""},
		{"absolute", &config.Config{ConfigPath: "/etc/sage/config.yaml",
			Policy: config.PolicyConfig{Directory: abs}}, abs},
		{"relative to the file", &config.Config{
			ConfigPath: filepath.Join("etc", "sage", "config.yaml"),
			Policy:     config.PolicyConfig{Directory: "policy"}},
			filepath.Join("etc", "sage", "policy")},
		{"relative without a file", &config.Config{
			Policy: config.PolicyConfig{Directory: " policy "}}, "policy"},
	}
	for _, tc := range cases {
		if got := gitopsDirectory(tc.cfg); got != tc.want {
			t.Errorf("%s: directory = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGitopsLoop_MissingInvalidAndUnreachable(t *testing.T) {
	root := t.TempDir()
	g := &gitopsLoop{dir: filepath.Join(root, "absent"), mode: gitops.ModeApply,
		databases: func() []string { return nil }, leader: func() bool { return true }}
	g.tick(context.Background())
	if r := gitopsStatus.Report(); !r.Missing || !g.failed {
		t.Fatalf("missing directory report = %+v, failed=%v", r, g.failed)
	}
	g.dir = root
	if err := os.MkdirAll(filepath.Join(root, "principals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "principals", "x.yaml"),
		[]byte("kind: Robot\nschema_version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.tick(context.Background())
	r := gitopsStatus.Report()
	if len(r.Problems) != 1 || r.Problems[0].File != "principals/x.yaml" ||
		!strings.Contains(r.Error, "unknown kind") {
		t.Fatalf("invalid directory report = %+v", r)
	}
	if err := os.Remove(filepath.Join(root, "principals", "x.yaml")); err != nil {
		t.Fatal(err)
	}
	g.tick(context.Background()) // valid, but no control database
	if r := gitopsStatus.Report(); r.Error == "" || r.Applied || !g.failed ||
		g.lastHash != "" {
		t.Fatalf("unreachable control database report = %+v", r)
	}
}

// A follower that becomes the leader reconciles at its next poll, not at
// the next resync: as a follower it wrote nothing for the same hash.
func TestGitopsLoop_LeadershipGainedReconcilesAtOnce(t *testing.T) {
	root := t.TempDir()
	dir, err := gitops.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	leader := false
	g := &gitopsLoop{dir: root, mode: gitops.ModeApply, lastHash: dir.Hash,
		databases: func() []string { return nil }, leader: func() bool { return leader }}
	g.tick(context.Background()) // follower, unchanged files: nothing to do
	if g.failed {
		t.Fatal("an unchanged directory was reconciled on a follower")
	}
	leader = true
	g.tick(context.Background()) // reconciles; no control database here
	if r := gitopsStatus.Report(); !g.failed || r.Error == "" || r.Directory != root {
		t.Fatalf("leadership gained: failed=%v report=%+v, want a reconcile attempt",
			g.failed, gitopsStatus.Report())
	}
}
