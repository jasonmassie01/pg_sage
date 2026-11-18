package workpack

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// Export writes p's export tier (evid-02c): the local tier with value
// samples removed (stats.json keeps the four numeric fields) and its
// charts, as a gzip tar sealed with evidence.SealFiles in format
// pg_sage-workload/1 and signed with key. It verifies with evidence.Verify
// and with `sha256sum -c SHA256SUMS`. It holds the local tier's statement
// texts, so it is for a person (an admin download), never a model. Without
// a key it is refused: there is no unsigned export.
func Export(p *Pack, key ed25519.PrivateKey, now time.Time) ([]byte, error) {
	if p == nil {
		return nil, errors.New("workpack: no pack to export")
	}
	if key == nil {
		return nil, ErrNoSigningKey
	}
	files := make([]evidence.File, 0, len(p.Files)+2*ChartFamilies)
	for _, f := range p.Files {
		if f.Name == fileStats {
			data, err := statsWithoutSamples(f.Data)
			if err != nil {
				return nil, err
			}
			f = evidence.File{Name: f.Name, Data: data}
		}
		files = append(files, f)
	}
	charts, err := chartsOf(p)
	if err != nil {
		return nil, err
	}
	sealed := evidence.SealFiles(Format, append(files, charts...), key, now)
	var buf bytes.Buffer
	if err := sealed.WriteTarGz(&buf); err != nil {
		return nil, fmt.Errorf("workpack: write the export of %s: %w", p.ContentSHA256, err)
	}
	return buf.Bytes(), nil
}

// statsWithoutSamples is stats.json with its value samples removed.
func statsWithoutSamples(data []byte) ([]byte, error) {
	trees, err := decodeTrees([]evidence.File{{Name: fileStats, Data: data}})
	if err != nil {
		return nil, err
	}
	tree := trees[fileStats]
	dropValueSamples(tree)
	return canonical(tree)
}
