// Package evidence builds auditor evidence packs (E2, spec §6.17, AU-4):
// an exportable .tar.gz holding everything pg_sage recorded about one
// change, or about a time window, with each file's SHA-256 in a manifest
// and a SHA256SUMS file (sha256sum -c), the hash-chain proof of the audit
// rows it contains and, when a signing key is configured, an Ed25519
// signature over the manifest.
package evidence

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// FormatV1 names the pack layout.
const FormatV1 = "pg_sage-evidence/1"

// maxWindow bounds a window pack; maxWindowRows bounds each of its files.
const (
	maxWindow     = 31 * 24 * time.Hour
	maxWindowRows = 10000
)

// Errors.
var (
	ErrScope    = errors.New("evidence: invalid scope")
	ErrNotFound = errors.New("evidence: no such action")
)

// Scope selects a pack: one action, or a time window [From, To).
type Scope struct {
	Database string    `json:"database,omitempty"`
	ActionID int64     `json:"action_id,omitempty"`
	From     time.Time `json:"from,omitzero"`
	To       time.Time `json:"to,omitzero"`
}

func (s Scope) validate() error {
	window := !s.From.IsZero() || !s.To.IsZero()
	switch {
	case s.ActionID < 0:
		return fmt.Errorf("%w: action id must be positive", ErrScope)
	case s.ActionID > 0 && window:
		return fmt.Errorf("%w: give an action or a window, not both", ErrScope)
	case s.ActionID == 0 && !window:
		return fmt.Errorf("%w: give an action or a window", ErrScope)
	case window && !s.From.Before(s.To):
		return fmt.Errorf("%w: the window must start before it ends", ErrScope)
	case window && s.To.Sub(s.From) > maxWindow:
		return fmt.Errorf("%w: the window may span at most 31 days", ErrScope)
	}
	return nil
}

// FileEntry is one file of a pack with its hash.
type FileEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Signer identifies the key that signed a manifest.
type Signer struct {
	Alg       string `json:"alg"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// Manifest describes a pack.
type Manifest struct {
	Format    string      `json:"format"`
	Generator string      `json:"generator"`
	CreatedAt time.Time   `json:"created_at"`
	Scope     Scope       `json:"scope"`
	ChainOK   bool        `json:"chain_ok"`
	Truncated bool        `json:"truncated"`
	Files     []FileEntry `json:"files"`
	Signer    *Signer     `json:"signer,omitempty"`
}

// File is one named file of a pack.
type File struct {
	Name string
	Data []byte
}

// Pack is a built evidence pack.
type Pack struct {
	Manifest Manifest
	Files    []File
	key      ed25519.PrivateKey
}

// Build collects the pack of scope from q. key nil leaves it unsigned.
func Build(ctx context.Context, q auditchain.Querier, scope Scope,
	key ed25519.PrivateKey, now time.Time) (*Pack, error) {
	if q == nil {
		return nil, fmt.Errorf("evidence: no database")
	}
	if err := scope.validate(); err != nil {
		return nil, err
	}
	b := &builder{q: q, pack: &Pack{key: key, Manifest: Manifest{Format: FormatV1,
		Generator: "pg_sage", CreatedAt: now.UTC(), Scope: scope, ChainOK: true}}}
	var err error
	if scope.ActionID > 0 {
		err = b.action(ctx, scope.ActionID)
	} else {
		err = b.window(ctx, scope.From, scope.To)
	}
	if err != nil {
		return nil, err
	}
	sort.Slice(b.pack.Files, func(i, j int) bool {
		return b.pack.Files[i].Name < b.pack.Files[j].Name
	})
	for _, f := range b.pack.Files {
		sum := sha256.Sum256(f.Data)
		b.pack.Manifest.Files = append(b.pack.Manifest.Files, FileEntry{Name: f.Name,
			SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(f.Data))})
	}
	if key != nil {
		pub := key.Public().(ed25519.PublicKey)
		b.pack.Manifest.Signer = &Signer{Alg: "Ed25519", KeyID: KeyID(pub),
			PublicKey: hex.EncodeToString(pub)}
	}
	return b.pack, nil
}

type builder struct {
	q    auditchain.Querier
	pack *Pack
}

func (b *builder) add(name string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("evidence: encode %s: %w", name, err)
	}
	b.pack.Files = append(b.pack.Files, File{Name: name, Data: append(body, '\n')})
	return nil
}

// rows runs a jsonb_agg query and adds its result as a file. A table that
// does not exist (yet) yields an empty list.
func (b *builder) rows(ctx context.Context, name, table, where string,
	args ...any) error {
	var exists bool
	if err := b.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT to_regclass($1)
		IS NOT NULL`, table).Scan(&exists); err != nil {
		return fmt.Errorf("evidence: %s: %w", name, err)
	}
	if !exists {
		return b.add(name, []any{})
	}
	sql := fmt.Sprintf(`/* pg_sage evidence v1 */ SELECT coalesce(jsonb_agg(x.j
		ORDER BY x.o), '[]'::jsonb) FROM (SELECT to_jsonb(t) AS j, to_jsonb(t)::text AS o
		FROM %s t WHERE %s LIMIT %d) x`, table, where, maxWindowRows+1)
	var raw []byte
	if err := b.q.QueryRow(ctx, sql, args...).Scan(&raw); err != nil {
		return fmt.Errorf("evidence: %s: %w", name, err)
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("evidence: %s: %w", name, err)
	}
	if len(list) > maxWindowRows {
		list, b.pack.Manifest.Truncated = list[:maxWindowRows], true
	}
	return b.add(name, list)
}

// action collects one change: request, decision and policy version,
// approvals, verification, outcome, pgaudit corroboration and chain proof.
func (b *builder) action(ctx context.Context, id int64) error {
	var raw []byte
	err := b.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT to_jsonb(a)
		FROM sage.action_log a WHERE a.id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("evidence: action %d: %w", id, err)
	}
	if err := b.add("action.json", json.RawMessage(raw)); err != nil {
		return err
	}
	related := []struct{ name, table, where string }{
		{"decision.json", "sage.decision", decisionFilter("t")},
		{"policy.json", "sage.policy", "t.id IN (SELECT d.policy_id FROM sage.decision d " +
			"WHERE " + decisionFilter("d") + ")"},
		{"queue.json", "sage.action_queue", "t.action_log_id = $1"},
		{"verification.json", "sage.verification", "t.action_log_id = $1"},
		{"outcome.json", "sage.action_outcome", "t.action_log_id = $1"},
		{"pgaudit.json", "sage.guard_pgaudit_events", "t.action_id = $1"},
	}
	for _, r := range related {
		if err := b.rows(ctx, r.name, r.table, r.where, id); err != nil {
			return err
		}
	}
	return b.actionChain(ctx, id)
}

// decisionFilter selects the decisions of action $1 under alias.
func decisionFilter(alias string) string {
	return alias + ".id = (SELECT decision_id FROM sage.action_log WHERE id = $1) OR " +
		alias + ".action_log_id = $1"
}
