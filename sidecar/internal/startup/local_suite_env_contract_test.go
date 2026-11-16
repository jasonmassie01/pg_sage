package startup

import (
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ci-19: the local runner's Go containers get every variable that ci.yml's
// test job sets for its steps, so a server CI gives the suite (ci-12's
// exclusive server, SAGE_TEST_EXCLUSIVE_URL) cannot go missing locally,
// where its tests would only skip.
//
// No concurrent-access or PostgreSQL surface: these tests read committed
// files. The servers themselves start in the script's real run.

const localSuiteServerScript = "local-linux-suite-server.sh"

// ciJobEnv returns the job-level env of one ci.yml job.
func ciJobEnv(t *testing.T, job string) map[string]string {
	t.Helper()
	var wf struct {
		Jobs map[string]struct {
			Env map[string]string `yaml:"env"`
		} `yaml:"jobs"`
	}
	path := ciWorkflowPath(t)
	if err := yaml.Unmarshal([]byte(readRepoFile(t, path)), &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	j, ok := wf.Jobs[job]
	if !ok {
		t.Fatalf("%s has no %s job", path, job)
	}
	return j.Env
}

// missingContainerEnv names, in order, every variable in names that the
// script never passes to a container as -e NAME=... or -e "NAME=...".
func missingContainerEnv(names []string, script string) []string {
	var missing []string
	for _, n := range names {
		if !strings.Contains(script, "-e "+n+"=") && !strings.Contains(script, `-e "`+n+"=") {
			missing = append(missing, n)
		}
	}
	return missing
}

func readLocalSuiteRunner(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "scripts", "ci")
	return readRepoFile(t, filepath.Join(dir, localSuiteScript)) + "\n" +
		readRepoFile(t, filepath.Join(dir, localSuiteServerScript))
}

func TestLocalSuitePassesCITestJobEnv(t *testing.T) {
	names := slices.Sorted(maps.Keys(ciJobEnv(t, "test")))
	for _, want := range []string{"SAGE_TEST_DATABASE_URL", "SAGE_TEST_EXCLUSIVE_URL"} {
		if !slices.Contains(names, want) {
			t.Fatalf("ci.yml's test job env %q lacks %s", names, want)
		}
	}
	if missing := missingContainerEnv(names, readLocalSuiteRunner(t)); len(missing) > 0 {
		t.Fatalf("the local runner does not pass ci.yml's test job env %q", missing)
	}
}

func TestLocalSuiteEnvDriftNamesDroppedExclusive(t *testing.T) {
	names := slices.Sorted(maps.Keys(ciJobEnv(t, "test")))
	script := readLocalSuiteRunner(t)
	dropped := strings.ReplaceAll(script, `-e "SAGE_TEST_EXCLUSIVE_URL=`, "")
	if dropped == script {
		t.Fatal("the runner has no -e \"SAGE_TEST_EXCLUSIVE_URL=...\" to drop")
	}
	got := missingContainerEnv(names, dropped)
	if !slices.Equal(got, []string{"SAGE_TEST_EXCLUSIVE_URL"}) {
		t.Fatalf("dropping the exclusive URL gave %q, want only SAGE_TEST_EXCLUSIVE_URL", got)
	}
}

func TestLocalSuiteMissingContainerEnvCases(t *testing.T) {
	cases := []struct {
		name, script string
		names, want  []string
	}{
		{"plain", "-e A=1 -e B=2", []string{"A", "B"}, nil},
		{"quoted", `-e "A=$X" -e B=2`, []string{"A", "B"}, nil},
		{"missing in order", "-e B=2", []string{"A", "B", "C"}, []string{"A", "C"}},
		{"prefix is not the name", "-e AB=1", []string{"A"}, []string{"A"}},
		{"named without -e", "A=1 go test", []string{"A"}, []string{"A"}},
		{"empty script", "", []string{"A"}, []string{"A"}},
		{"no names", "-e A=1", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := missingContainerEnv(c.names, c.script); !slices.Equal(got, c.want) {
				t.Fatalf("missingContainerEnv = %q, want %q", got, c.want)
			}
		})
	}
}
