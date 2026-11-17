// Package evidence builds auditor evidence packs (E2, spec §6.17, AU-4):
// an exportable .tar.gz holding everything pg_sage recorded about one
// change, or about a time window, with each file's SHA-256 in a manifest
// and a SHA256SUMS file (sha256sum -c), the hash-chain proof of the audit
// rows it contains and, when a signing key is configured or generated
// (key.go), an Ed25519 signature over the manifest.
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

// Format names the pack layout. Version 2 (audit-07) adds each file's
// source, row count, truncation and last id, continuation scopes, the
// guard query audit of a window and the policy approvals of an action.
const Format = "pg_sage-evidence/2"

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

// Scope selects a pack: one action, or a time window [From, To). A
// continuation of a window names one of its files and holds only that
// file's rows above AfterID, with their chain proof.
type Scope struct {
	Database string    `json:"database,omitempty"`
	ActionID int64     `json:"action_id,omitempty"`
	From     time.Time `json:"from,omitzero"`
	To       time.Time `json:"to,omitzero"`
	File     string    `json:"file,omitempty"`
	AfterID  int64     `json:"after_id,omitempty"`
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
	return s.validatePage()
}

// validatePage checks a continuation: a window file, a non-negative id.
func (s Scope) validatePage() error {
	switch {
	case s.AfterID < 0:
		return fmt.Errorf("%w: after_id must not be negative", ErrScope)
	case s.AfterID > 0 && s.File == "":
		return fmt.Errorf("%w: after_id needs the file it pages", ErrScope)
	case s.File == "":
		return nil
	case s.ActionID > 0:
		return fmt.Errorf("%w: only window packs are paged; leave out file", ErrScope)
	}
	for _, f := range windowFiles {
		if f.name == s.File {
			return nil
		}
	}
	return fmt.Errorf("%w: %q is not a file of a window pack", ErrScope, s.File)
}

// FileEntry is one file of a pack with its hash. Source is the database
// its rows were read from ("database", the audited one, or "control");
// a file of rows also has its count, whether the row bound cut it and the
// id of its last row (the after_id of its continuation).
type FileEntry struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	Bytes     int64  `json:"bytes"`
	Source    string `json:"source,omitempty"`
	Rows      *int   `json:"rows,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	LastID    *int64 `json:"last_id,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Signer identifies the key that signed a manifest.
type Signer struct {
	Alg       string `json:"alg"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// Manifest describes a pack. Truncated is true when any file was cut.
type Manifest struct {
	Format    string    `json:"format"`
	Generator string    `json:"generator"`
	CreatedAt time.Time `json:"created_at"`
	Scope     Scope     `json:"scope"`
	ChainOK   bool      `json:"chain_ok"`
	Truncated bool      `json:"truncated"`
	// PolicyHashMatch reports, for an action pack, whether every policy
	// hash its decisions recorded matches the document in policy.json;
	// absent when no decision recorded one (policy-03).
	PolicyHashMatch *bool       `json:"policy_hash_match,omitempty"`
	Files           []FileEntry `json:"files"`
	Signer          *Signer     `json:"signer,omitempty"`
	// Signed is whether manifest.sig signs this manifest (audit-10). An
	// unsigned pack's Integrity is IntegrityHashOnly: anyone who edits a
	// file can regenerate its hashes, so they catch accidents, not forgery.
	Signed    bool   `json:"signed"`
	Integrity string `json:"integrity,omitempty"`
}

// IntegrityHashOnly is the Integrity of an unsigned manifest.
const IntegrityHashOnly = "accidental-corruption-only"

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
	meta     map[string]fileMeta
}

// Sources are the databases a pack reads. DB is the audited database: its
// actions, decisions, pgaudit and guard query audit. Control is the
// control database when it is not DB (fleet mode): it holds the sign-ins,
// config changes, standing policy and policy approvals. Control nil
// (standalone, or the control database's own pack) reads all from DB.
// DatabaseID is DB's sage.databases id, which selects its config changes
// in Control; 0 when unknown.
type Sources struct {
	DB         auditchain.Querier
	Control    auditchain.Querier
	DatabaseID int
}

// Build collects the pack of scope from src. key nil leaves it unsigned.
func Build(ctx context.Context, src Sources, scope Scope,
	key ed25519.PrivateKey, now time.Time) (*Pack, error) {
	if src.DB == nil {
		return nil, fmt.Errorf("evidence: no audited database")
	}
	if err := scope.validate(); err != nil {
		return nil, err
	}
	b := newBuilder(src, scope, key, now)
	var err error
	if scope.ActionID > 0 {
		err = b.action(ctx, scope.ActionID)
	} else {
		err = b.window(ctx, scope.From, scope.To)
	}
	if err != nil {
		return nil, err
	}
	b.pack.seal()
	return b.pack, nil
}

// SealFiles seals files as a pack of format: the manifest lists each, in
// name order, with its SHA-256, and key (nil: unsigned) signs it when the
// pack is written (WriteTarGz). It carries none of an audit pack's
// metadata (scope, chain, sources, rows); the workload evidence pack
// (evid-02a) is sealed with it. createdAt is the manifest's created_at.
func SealFiles(format string, files []File, key ed25519.PrivateKey,
	createdAt time.Time) *Pack {
	p := &Pack{key: key, meta: map[string]fileMeta{}, Files: append([]File(nil), files...),
		Manifest: Manifest{Format: format, Generator: "pg_sage", CreatedAt: createdAt.UTC()}}
	p.seal()
	return p
}

// seal lists every file in the manifest, in name order, with its hash.
func (p *Pack) seal() {
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Name < p.Files[j].Name })
	for _, f := range p.Files {
		sum := sha256.Sum256(f.Data)
		meta := p.meta[f.Name]
		p.Manifest.Files = append(p.Manifest.Files, FileEntry{Name: f.Name,
			SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(f.Data)),
			Source: meta.source, Rows: meta.rows, Truncated: meta.truncated,
			LastID: meta.lastID, Note: meta.note})
		p.Manifest.Truncated = p.Manifest.Truncated || meta.truncated
	}
	p.Manifest.Signed = p.key != nil
	if p.key == nil {
		p.Manifest.Integrity = IntegrityHashOnly
		return
	}
	pub := p.key.Public().(ed25519.PublicKey)
	p.Manifest.Signer = &Signer{Alg: "Ed25519", KeyID: KeyID(pub),
		PublicKey: hex.EncodeToString(pub)}
}

// Source names in a manifest.
const (
	sourceDatabase = "database"
	sourceControl  = "control"
)

// source is a database a pack reads, with its manifest name.
type source struct {
	q    auditchain.Querier
	name string
}

func (s source) describe() string {
	if s.name == sourceControl {
		return "the control database"
	}
	return "the audited database"
}

// fileMeta is what a manifest entry says about a file beyond its hash.
type fileMeta struct {
	source    string
	rows      *int
	truncated bool
	lastID    *int64
	note      string
}

type builder struct {
	db, control source
	databaseID  int
	scope       Scope
	pack        *Pack
	// policyIDs are the policy versions in an action pack's policy.json;
	// approvalIDs their approvals' ids (nil: no approvals table).
	policyIDs, approvalIDs []int64
}

func newBuilder(src Sources, scope Scope, key ed25519.PrivateKey, now time.Time) *builder {
	b := &builder{db: source{q: src.DB, name: sourceDatabase}, databaseID: src.DatabaseID,
		scope: scope, pack: &Pack{key: key, meta: map[string]fileMeta{},
			Manifest: Manifest{Format: Format, Generator: "pg_sage",
				CreatedAt: now.UTC(), Scope: scope, ChainOK: true}}}
	b.control = b.db
	if src.Control != nil {
		b.control = source{q: src.Control, name: sourceControl}
	}
	return b
}

// fleet is whether the control database is not the audited one.
func (b *builder) fleet() bool { return b.control.name == sourceControl }

func (b *builder) add(name string, meta fileMeta, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("evidence: encode %s: %w", name, err)
	}
	b.pack.Files = append(b.pack.Files, File{Name: name, Data: append(body, '\n')})
	b.pack.meta[name] = meta
	return nil
}

// rowQuery selects the rows of one file: table t under where, from src,
// ordered by its key column (id unless named).
type rowQuery struct {
	name, table, where string
	key                string
	src                source
	args               []any
	note               string
}

func (q rowQuery) keyColumn() string {
	if q.key == "" {
		return "id"
	}
	return q.key
}

// rows adds q's rows as a file, in id order and at most maxWindowRows of
// them, and returns what it took. A table that does not exist (yet)
// yields an empty list and exists false.
func (b *builder) rows(ctx context.Context, q rowQuery) (fileMeta, bool, error) {
	meta := fileMeta{source: q.src.name, note: q.note}
	fail := func(err error) (fileMeta, bool, error) {
		return meta, false, fmt.Errorf("evidence: %s from %s: %w", q.name,
			q.src.describe(), err)
	}
	var exists bool
	if err := q.src.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT to_regclass($1)
		IS NOT NULL`, q.table).Scan(&exists); err != nil {
		return fail(err)
	}
	list, ids := []any{}, []int64{}
	if exists {
		sql := fmt.Sprintf(`/* pg_sage evidence v1 */ SELECT coalesce(jsonb_agg(x.j
		ORDER BY x.id), '[]'::jsonb), coalesce(array_agg(x.id ORDER BY x.id), '{}')
		FROM (SELECT to_jsonb(t) AS j, t.%[1]s::bigint AS id FROM %[2]s t WHERE (%[3]s)
		ORDER BY t.%[1]s LIMIT %[4]d) x`, q.keyColumn(), q.table, q.where, maxWindowRows+1)
		var raw []byte
		if err := q.src.q.QueryRow(ctx, sql, q.args...).Scan(&raw, &ids); err != nil {
			return fail(err)
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return fail(err)
		}
	}
	if len(list) > maxWindowRows {
		list, ids, meta.truncated = list[:maxWindowRows], ids[:maxWindowRows], true
	}
	n := len(list)
	meta.rows = &n
	if n > 0 {
		meta.lastID = &ids[n-1]
	}
	return meta, exists, b.add(q.name, meta, list)
}

// action collects one change: request, decision and the policy it names
// with its approvals, queue entry, verification, outcome, pgaudit
// corroboration, the plan a plan step's change belongs to, and chain proof.
func (b *builder) action(ctx context.Context, id int64) error {
	var raw []byte
	err := b.db.q.QueryRow(ctx, `/* pg_sage evidence v1 */ SELECT to_jsonb(a)
		FROM sage.action_log a WHERE a.id = $1`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("evidence: action %d: %w", id, err)
	}
	if err := b.add("action.json", fileMeta{source: sourceDatabase},
		json.RawMessage(raw)); err != nil {
		return err
	}
	related := []struct{ name, table, where, key string }{
		{"decision.json", "sage.decision", decisionFilter("t"), ""},
		{"queue.json", "sage.action_queue", "t.action_log_id = $1", ""},
		{"verification.json", "sage.verification", "t.action_log_id = $1", ""},
		// sage.action_outcome is keyed by its action.
		{"outcome.json", "sage.action_outcome", "t.action_log_id = $1", "action_log_id"},
		{"pgaudit.json", "sage.guard_pgaudit_events", pgauditFilter, ""},
	}
	for _, r := range related {
		q := rowQuery{name: r.name, table: r.table, where: r.where, key: r.key,
			src: b.db, args: []any{id}}
		if _, _, err := b.rows(ctx, q); err != nil {
			return err
		}
	}
	if err := b.policies(ctx, id); err != nil {
		return err
	}
	if err := b.plans(ctx, id); err != nil {
		return err
	}
	return b.actionChain(ctx, id)
}

// decisionFilter selects the decisions of action $1 under alias.
func decisionFilter(alias string) string {
	return alias + ".id = (SELECT decision_id FROM sage.action_log WHERE id = $1) OR " +
		alias + ".action_log_id = $1"
}

// pgauditFilter selects the pgaudit records of action $1: those linked to
// it, and those its decision's tagged session logged (audit-06), which may
// have been stored before the action row existed, with no action_id.
const pgauditFilter = "t.action_id = $1 OR (t.correlated_by = 'action_tag' AND " +
	"t.application = 'pg_sage action:' || " +
	"(SELECT decision_id FROM sage.action_log WHERE id = $1))"
