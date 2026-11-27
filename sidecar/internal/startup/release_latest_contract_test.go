package startup

import (
	"strings"
	"testing"
)

// ci-05: :latest is the newest stable release. A v*-rc tag (the CHECK-21
// dry run) publishes :X.Y.Z-rcN only; it never moves :latest, and master
// never does either. Both metadata steps carry the rule: the docker legs
// label their images with it and docker-manifest applies the tags.

// No concurrent-access tests: this reads a committed workflow file.

func TestLatestTagSkipsPrereleases(t *testing.T) {
	wf := loadRelWorkflow(t)
	refs := []struct {
		ref  string
		want bool
	}{
		{"refs/tags/v2.6.0", true},
		{"refs/tags/v10.0.1", true},
		{"refs/tags/v2.6.0-rc1", false},
		{"refs/tags/v2.6.0-RC.2", false},
		{"refs/tags/v2.6.0-beta.1", false},
		{"refs/heads/master", false},
		{"refs/pull/12/merge", false},
	}
	for _, job := range []string{"docker", "docker-manifest"} {
		cond := latestEnable(t, relJobNamed(t, wf, job), job)
		for _, r := range refs {
			got, err := ghIf(cond, pushState(r.ref, nil))
			if err != nil {
				t.Fatalf("%s :latest enable %q: %v", job, cond, err)
			}
			if got != r.want {
				t.Errorf("%s moves :latest on %s = %v, want %v (enable=%s)",
					job, r.ref, got, r.want, cond)
			}
		}
	}
}

// latestEnable returns the enable= expression of the meta step's
// type=raw,value=latest rule. It fails the test when the step is missing,
// when metadata-action may add :latest on its own (flavor latest=false
// absent) or when the rule is not exactly one enable-gated line.
func latestEnable(t *testing.T, job relJob, name string) string {
	t.Helper()
	meta := -1
	for i, s := range job.Steps {
		if s.ID == "meta" && strings.HasPrefix(s.Uses, "docker/metadata-action") {
			meta = i
		}
	}
	if meta < 0 {
		t.Fatalf("%s has no metadata-action step with id meta", name)
	}
	with := job.Steps[meta].With
	if strings.TrimSpace(with["flavor"]) != "latest=false" {
		t.Fatalf("%s meta flavor %q, want latest=false (only the explicit rule adds :latest)",
			name, with["flavor"])
	}
	var rules []string
	for _, line := range strings.Split(with["tags"], "\n") {
		if strings.Contains(line, "latest") {
			rules = append(rules, strings.TrimSpace(line))
		}
	}
	const prefix = "type=raw,value=latest,enable="
	if len(rules) != 1 || !strings.HasPrefix(rules[0], prefix) {
		t.Fatalf("%s meta tags name :latest in %q, want one %s... rule", name, rules, prefix)
	}
	return strings.TrimPrefix(rules[0], prefix)
}
