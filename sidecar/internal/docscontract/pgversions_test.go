package docscontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// footprint-10b: every supported-PostgreSQL claim in the guides agrees with
// the majors CI runs the integration tests on.
//
// No concurrent-access, state-transition or integration tests: these are
// static checks over ci.yml and the Markdown pages.

func TestPGVersionsCIMatrixIs14To18(t *testing.T) {
	path := filepath.Join(repoRoot(t), ".github", "workflows", "ci.yml")
	majors, err := readCIMajors(path)
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(majors, []int{14, 15, 16, 17, 18}) {
		t.Fatalf("CI tests PostgreSQL %v; the guides say 14-18, so update them "+
			"and this pin together", majors)
	}
}

func TestPGVersionsDocsMatchCIMatrix(t *testing.T) {
	root := repoRoot(t)
	majors, err := readCIMajors(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	pages, err := goRequirementPages(root)
	if err != nil {
		t.Fatal(err)
	}
	var problems []claimProblem
	claims := 0
	for _, page := range pages {
		body, err := os.ReadFile(page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		rel, err := filepath.Rel(root, page)
		if err != nil {
			t.Fatalf("relative path of %s: %v", page, err)
		}
		found, n := versionClaimProblems(filepath.ToSlash(rel), string(body), majors)
		problems = append(problems, found...)
		claims += n
	}
	if claims < 40 {
		t.Fatalf("read only %d PostgreSQL version claims; the guides were not scanned", claims)
	}
	kept, unused := withoutVersionSpecific(problems, versionSpecificClaims)
	for _, p := range kept {
		t.Errorf("%s", p)
	}
	for _, key := range unused {
		t.Errorf("versionSpecificClaims entry %q matches no claim; remove it", key)
	}
}

func TestPGVersionsSelfManagedRowIsTheCIMatrix(t *testing.T) {
	root := repoRoot(t)
	majors, err := readCIMajors(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "docs", "installation.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := selfManagedRowProblem(string(body), majors); err != nil {
		t.Errorf("%s: %v", path, err)
	}
}

const ciFixture = `jobs:
  test:
    services:
      postgres:
        image: ghcr.io/x/pg_sage-ci-pgvector:0.8.2-pg17-r1
  integration-matrix:
    strategy:
      matrix:
        pg: [%s]
  docker:
    strategy:
      matrix:
        include:
          - platform: linux/amd64
`

func ciDoc(pg string) []byte {
	return []byte(strings.Replace(ciFixture, "%s", pg, 1))
}

func TestPGVersionsCIMajorsParsesTheMatrix(t *testing.T) {
	cases := []struct {
		pg   string
		want []int
	}{
		{`"14", "15", "16", "18"`, []int{14, 15, 16, 17, 18}},
		{`14, 15, 16, 17, 18`, []int{14, 15, 16, 17, 18}},
		{`"16", "18"`, []int{16, 17, 18}},
		{`"17"`, []int{17}},
	}
	for _, tc := range cases {
		got, err := ciMajors(ciDoc(tc.pg))
		if err != nil || !equalInts(got, tc.want) {
			t.Errorf("ciMajors(pg: [%s]) = %v, %v; want %v", tc.pg, got, err, tc.want)
		}
	}
}

func TestPGVersionsCIMajorsRejectsBadMatrices(t *testing.T) {
	noTestJob := "jobs:\n  integration-matrix:\n    strategy:\n      matrix:\n" +
		"        pg: [\"14\"]\n"
	cases := []struct {
		name string
		doc  []byte
		want string
	}{
		{"not a number", ciDoc(`"14", "18beta"`), `entry "18beta" is not a PostgreSQL major`},
		{"zero", ciDoc(`"0"`), `entry "0" is not a PostgreSQL major`},
		{"empty list", ciDoc(``), "ci.yml integration matrix not found"},
		{"no matrix job", []byte("jobs:\n  test:\n    runs-on: x\n"),
			"ci.yml integration matrix not found"},
		{"empty file", nil, "ci.yml integration matrix not found"},
		{"no test image", []byte(noTestJob), "names no major"},
		{"gap", ciDoc(`"14", "15"`), "not a contiguous range"},
		{"not yaml", []byte("jobs: [\n"), "parse ci.yml"},
	}
	for _, tc := range cases {
		got, err := ciMajors(tc.doc)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ciMajors = %v, %v; want an error containing %q",
				tc.name, got, err, tc.want)
		}
	}
}

func TestPGVersionsReadCIMajorsNamesThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-ci.yml")
	if _, err := readCIMajors(missing); err == nil ||
		!strings.Contains(err.Error(), missing) {
		t.Fatalf("readCIMajors(%s) error = %v; want one naming the path", missing, err)
	}
	bad := filepath.Join(t.TempDir(), "ci.yml")
	if err := os.WriteFile(bad, ciDoc(`"18beta"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readCIMajors(bad)
	if err == nil || !strings.Contains(err.Error(), bad) ||
		!strings.Contains(err.Error(), "18beta") {
		t.Fatalf("readCIMajors(%s) error = %v; want one naming the path and entry", bad, err)
	}
}
