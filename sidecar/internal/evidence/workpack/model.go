package workpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence"
)

// The redaction tiers of a pack (evid-02c). The local tier is what Build
// stores: full, it never leaves. The model tier (ForModel) is what a model
// may read: numeric statistics and redacted text only, identifiers
// optionally hashed, no most-common values and no plan filter values, with
// sparklines of the top families. The export tier (Export) is the local
// tier without value samples, signed, for a person.

// Tier names.
const (
	TierLocal  = "local"
	TierModel  = "model"
	TierExport = "export"
)

// Model-tier bounds.
const (
	// MaxModelTextChars bounds a statement text; MaxModelNoteChars an
	// owner note.
	MaxModelTextChars = 2000
	MaxModelNoteChars = 300
	// ChartFamilies is how many families (by exec time) get charts.
	ChartFamilies = 10
	// PseudonymKeyBytes is the size of the pseudonym key.
	PseudonymKeyBytes = 32
)

// Errors a caller can tell apart.
var (
	// ErrModelLeak means a string of the derived model tier holds a value
	// the local tier read (a most-common value, a histogram bound, a string
	// literal) or a credential-shaped token. No model tier is returned.
	ErrModelLeak = errors.New("workpack: the model tier would leak local-tier data")
	// ErrNoPseudonymKey means HashIdentifiers was asked without the
	// install's pseudonym key, which lives sealed under the encryption
	// keyring.
	ErrNoPseudonymKey = errors.New("workpack: hashing identifiers needs the pseudonym " +
		"key, stored sealed under the encryption keyring: set encryption_key")
	// ErrNoSigningKey means an export was asked with no evidence signing
	// key; an export is never unsigned.
	ErrNoSigningKey = errors.New("workpack: no evidence signing key: an export is " +
		"always signed; set encryption_key or audit.evidence.signing_key_env")
)

// ModelOptions shape a model tier. HashIdentifiers replaces every schema,
// table, index and column name with t_<10 hex> of an HMAC-SHA256 keyed by
// PseudonymKey (LoadOrCreatePseudonymKey); off by default.
type ModelOptions struct {
	HashIdentifiers bool
	PseudonymKey    []byte
}

// ForModel derives p's model tier: a pure function of p and opts with its
// own content hash. p is not changed. A leak the guard finds returns
// ErrModelLeak naming the section and field, never the value.
func ForModel(p *Pack, opts ModelOptions) (*Pack, error) {
	if p == nil {
		return nil, errors.New("workpack: no pack to derive a model tier from")
	}
	if opts.HashIdentifiers && len(opts.PseudonymKey) != PseudonymKeyBytes {
		return nil, ErrNoPseudonymKey
	}
	local, err := decodeTrees(p.Files)
	if err != nil {
		return nil, err
	}
	names := collectNames(local)
	model, err := decodeTrees(p.Files)
	if err != nil {
		return nil, err
	}
	var ps *pseudonymizer
	if opts.HashIdentifiers {
		ps = newPseudonymizer(opts.PseudonymKey, names)
	}
	for _, name := range sortedNames(model) {
		model[name] = modelRedact(name, model[name])
		if ps != nil {
			model[name] = ps.walk(model[name], "")
		}
	}
	if err := checkLeaks(model, leakGuard{needles: leakNeedles(local, names, model),
		names: names}); err != nil {
		return nil, err
	}
	model[fileTier] = ModelTier{Format: Format, Tier: TierModel,
		SourceContentSHA256: p.ContentSHA256, HashIdentifiers: opts.HashIdentifiers}
	return sealModel(p, model, ps)
}

// fileTier names the model tier's provenance file.
const fileTier = "tier.json"

// ModelTier says what a model tier is: the local pack it was derived from
// (the pack its evidence ids resolve against) and whether its names are
// pseudonyms. It also gives every model tier a hash of its own.
type ModelTier struct {
	Format              string `json:"format"`
	Tier                string `json:"tier"`
	SourceContentSHA256 string `json:"source_content_sha256"`
	HashIdentifiers     bool   `json:"hash_identifiers"`
}

// sealModel encodes the model sections, adds the charts and seals them.
func sealModel(p *Pack, model map[string]any, ps *pseudonymizer) (*Pack, error) {
	files := make([]evidence.File, 0, len(model)+2*ChartFamilies)
	for _, name := range sortedNames(model) {
		data, err := canonical(model[name])
		if err != nil {
			return nil, fmt.Errorf("model %s: %w", name, err)
		}
		files = append(files, evidence.File{Name: name, Data: data})
	}
	charts, err := chartsOf(p)
	if err != nil {
		return nil, err
	}
	files = append(files, charts...)
	sealed := evidence.SealFiles(Format, files, nil, time.Time{})
	sums := map[string]string{}
	for _, e := range sealed.Manifest.Files {
		sums[e.Name] = e.SHA256
	}
	body, err := tarGz(sealed.Files)
	if err != nil {
		return nil, err
	}
	m := Manifest{Format: Format, ContentSHA256: contentSHA256(sums),
		Database: p.Manifest.Database, Window: p.Window, Files: sealed.Manifest.Files,
		Tier: TierModel}
	out := &Pack{Format: Format, ContentSHA256: m.ContentSHA256, Window: p.Window,
		Files: sealed.Files, Body: body, Manifest: m}
	if ps != nil {
		out.Pseudonyms = ps.out
	}
	return out, nil
}

// decodeTrees decodes every JSON section of files, numbers kept exact.
func decodeTrees(files []evidence.File) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range files {
		if !strings.HasSuffix(f.Name, ".json") {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(f.Data))
		dec.UseNumber()
		var tree any
		if err := dec.Decode(&tree); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrCorrupt, f.Name, err)
		}
		out[f.Name] = tree
	}
	return out, nil
}

func sortedNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// objects is tree[key] as a list of objects (nil when it is not one).
func objects(tree any, key string) []map[string]any {
	root, _ := tree.(map[string]any)
	list, _ := root[key].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
