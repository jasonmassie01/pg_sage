package buildtags

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// BuiltByTests maps each custom tag that no CI recipe may name to the
// module-relative test file that passes it to go build. The e2e suite builds
// the sidecar binary with sage_e2e_assume_sqlast on a host without cgo
// (executor-10), and that tag must never appear in a CI recipe
// (TestNoBuildRecipeUsesTheSQLASTAssumptionTag), so the vet step cannot list
// it. CI builds with cgo and never passes the tag, so the unit suite vets
// every package that uses it instead (TestTagsBuiltByTestsAreVetted, ci-18).
var BuiltByTests = map[string]string{
	"sage_e2e_assume_sqlast": "e2e/pipeline_binary_gate_test.go",
}

// TagsBuiltByTests returns, sorted, the tags of built whose test file under
// root still names them. A missing file, or one that no longer names its
// tag, is an error: the exemption cannot outlive the test that compiles it.
func TagsBuiltByTests(root string, built map[string]string) ([]string, error) {
	var tags []string
	for tag, rel := range built {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("tag %s is exempt from the vet step because %s builds "+
				"with it, but that file cannot be read: %w", tag, rel, err)
		}
		if !bytes.Contains(src, []byte(tag)) {
			return nil, fmt.Errorf("tag %s is exempt from the vet step because %s builds "+
				"with it, but that file no longer names it: list the tag in the vet step "+
				"or drop the exemption", tag, rel)
		}
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	return tags, nil
}
