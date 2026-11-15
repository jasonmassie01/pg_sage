package workpack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/histstore"
)

// Stored packs live in sage.workload_packs where the database's history
// lives (histstore), once per database and content hash. A stored pack
// never changes: Save inserts a new hash or returns the row already
// holding it, and nothing updates the table (retention only deletes).

// Stored is a sage.workload_packs row without its body. Reused is true
// when Save found the hash already stored.
type Stored struct {
	ID            int64           `json:"id"`
	Window        Window          `json:"window"`
	Format        string          `json:"format"`
	ContentSHA256 string          `json:"content_sha256"`
	Tier          string          `json:"tier"`
	Bytes         int64           `json:"bytes"`
	BuiltAt       time.Time       `json:"built_at"`
	BuiltBy       string          `json:"built_by"`
	Manifest      json.RawMessage `json:"manifest,omitempty"`
	Reused        bool            `json:"reused"`
}

// tierLocal is the only tier this release stores: the full pack, which
// never leaves the deployment.
const tierLocal = "local"

const insertSQL = `/* pg_sage */ INSERT INTO sage.workload_packs (window_from, window_to,
       format, content_sha256, tier, bytes, built_at, built_by, manifest, body{dbcol})
VALUES ($1, $2, $3, $4, '` + tierLocal + `', $5, $6, $7, $8, $9{dbval})
ON CONFLICT ((COALESCE(database_id, 0)), content_sha256) DO NOTHING
RETURNING id`

const rowColumns = `p.id, p.window_from, p.window_to, p.format, p.content_sha256, p.tier,
       p.bytes, p.built_at, p.built_by`

var byHashSQL = `/* pg_sage */ SELECT ` + rowColumns + `, p.manifest, p.body
  FROM sage.workload_packs p WHERE {db:p} AND p.content_sha256 = $1`

var listSQL = `/* pg_sage */ SELECT ` + rowColumns + `
  FROM sage.workload_packs p WHERE {db:p}
 ORDER BY p.built_at DESC, p.id DESC LIMIT $1`

// Save stores p for db's history store, built at builtAt by builtBy, or
// returns the stored row of its hash with Reused set.
func Save(ctx context.Context, db any, p *Pack, builtAt time.Time,
	builtBy string) (Stored, error) {
	st := histstore.Resolve(db)
	manifest, err := json.Marshal(p.Manifest)
	if err != nil {
		return Stored{}, fmt.Errorf("workpack: encode the manifest: %w", err)
	}
	var id int64
	err = st.QueryRow(ctx, insertSQL, p.Window.From, p.Window.To, p.Format,
		p.ContentSHA256, int64(len(p.Body)), builtAt.UTC(), builtBy, manifest,
		p.Body).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		row, _, err := load(ctx, st, p.ContentSHA256)
		row.Reused = true
		return row, err
	case err != nil:
		return Stored{}, fmt.Errorf("workpack: store pack %s in sage.workload_packs: %w",
			p.ContentSHA256, err)
	}
	return Stored{ID: id, Window: p.Window, Format: p.Format, ContentSHA256: p.ContentSHA256,
		Tier: tierLocal, Bytes: int64(len(p.Body)), BuiltAt: builtAt.UTC(), BuiltBy: builtBy,
		Manifest: manifest}, nil
}

// Load returns the stored pack of hash sha, verified against its manifest
// (ErrNotFound when db stores none).
func Load(ctx context.Context, db any, sha string) (Stored, *Pack, error) {
	return load(ctx, histstore.Resolve(db), sha)
}

func load(ctx context.Context, st histstore.Store, sha string) (Stored, *Pack, error) {
	var s Stored
	var body []byte
	err := st.QueryRow(ctx, byHashSQL, sha).Scan(&s.ID, &s.Window.From, &s.Window.To,
		&s.Format, &s.ContentSHA256, &s.Tier, &s.Bytes, &s.BuiltAt, &s.BuiltBy,
		&s.Manifest, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stored{}, nil, fmt.Errorf("%w: %s", ErrNotFound, sha)
	}
	if err != nil {
		return Stored{}, nil, fmt.Errorf("workpack: read pack %s from sage.workload_packs: %w",
			sha, err)
	}
	s.Window = s.Window.UTC()
	var m Manifest
	if err := json.Unmarshal(s.Manifest, &m); err != nil {
		return Stored{}, nil, fmt.Errorf("%w: manifest of %s: %v", ErrCorrupt, sha, err)
	}
	p, err := Decode(body, m)
	if err != nil {
		return Stored{}, nil, fmt.Errorf("pack %s: %w", sha, err)
	}
	return s, p, nil
}

// List returns db's stored packs, newest first, at most limit.
func List(ctx context.Context, db any, limit int) ([]Stored, error) {
	rows, err := histstore.Resolve(db).Query(ctx, listSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("workpack: list sage.workload_packs: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stored, error) {
		var s Stored
		err := row.Scan(&s.ID, &s.Window.From, &s.Window.To, &s.Format, &s.ContentSHA256,
			&s.Tier, &s.Bytes, &s.BuiltAt, &s.BuiltBy)
		s.Window = s.Window.UTC()
		return s, err
	})
	if err != nil {
		return nil, fmt.Errorf("workpack: list sage.workload_packs: %w", err)
	}
	return out, nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA reports a content hash's form: 64 lower-case hex digits.
func ValidSHA(s string) bool { return shaPattern.MatchString(s) }
