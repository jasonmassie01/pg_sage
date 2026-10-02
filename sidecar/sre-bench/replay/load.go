package replay

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// corpusFS is the embedded corpus: cases/<family>/<id>.json.
//
//go:embed cases
var corpusFS embed.FS

// Corpus loads and validates the embedded corpus against the probe
// catalog.
func Corpus() ([]Case, error) { return Load(corpusFS, probes.Catalog()) }

// Load reads every *.json file under fsys (recursively), parses and
// validates each case, and returns them sorted by id. Duplicate ids and
// an empty corpus are errors; an error names the file.
func Load(fsys fs.FS, reg *probes.Registry) ([]Case, error) {
	var out []Case
	seen := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".json" {
			return err
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		c, err := Parse(raw)
		if err == nil {
			err = c.Validate(reg)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if other, dup := seen[c.ID]; dup {
			return invalid("%s: duplicate id %q (also in %s)", p, c.ID, other)
		}
		seen[c.ID] = p
		out = append(out, c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, invalid("no cases found")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
