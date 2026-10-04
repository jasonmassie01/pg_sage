package agenttools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/facts"
)

// Mark kinds.
const (
	MarkOwned  = "owned"
	MarkExempt = "exempt"
)

const maxActor = 200

// MarkRequest marks an object as owned by the application's migrations
// (pg_sage then hands changes to it over as source fixes instead of
// running DDL) or exempt from pg_sage's changes.
type MarkRequest struct {
	Kind     string `json:"subject_kind"`
	Subject  string `json:"subject"`
	Mark     string `json:"mark"`
	Repo     string `json:"repo,omitempty"`
	Path     string `json:"path,omitempty"`
	Evidence string `json:"evidence"`
}

// MarkResult is the proposed fact.
type MarkResult struct {
	FactID  int64  `json:"fact_id"`
	Status  string `json:"status"`
	Created bool   `json:"created"`
	Summary string `json:"summary"`
	Note    string `json:"note"`
}

const markNote = "Proposed only: a mark binds nothing until an operator confirms the " +
	"fact (UI, API or an approve-scoped MCP token)."

// MarkObject proposes an owned_by_app_migrations fact (source operator,
// proposed by actor). It never confirms the fact. An index or table
// subject without '*' must exist.
func (t *Tools) MarkObject(ctx context.Context, req MarkRequest, actor string,
) (MarkResult, error) {
	if err := t.ready(); err != nil {
		return MarkResult{}, err
	}
	proposal, err := markProposal(req, actor, t.now)
	if err != nil {
		return MarkResult{}, err
	}
	pattern, err := facts.ParsePattern(proposal.Kind, req.Subject)
	if err != nil {
		return MarkResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := t.requireObject(ctx, pattern); err != nil {
		return MarkResult{}, err
	}
	fact, created, err := facts.NewStore(t.pool).WithLog(t.opts.Log).Propose(ctx, proposal)
	if err != nil {
		return MarkResult{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return MarkResult{FactID: fact.ID, Status: string(fact.Status), Created: created,
		Summary: fact.Describe(), Note: markNote}, nil
}

func markProposal(req MarkRequest, actor string, now func() time.Time) (facts.Proposal, error) {
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > maxActor {
		return facts.Proposal{}, invalid("an actor of 1..%d bytes is required", maxActor)
	}
	evidence := strings.TrimSpace(req.Evidence)
	if evidence == "" {
		return facts.Proposal{}, invalid("evidence is required")
	}
	kind := facts.Kind(req.Kind)
	if kind != facts.KindIndex && kind != facts.KindTable && kind != facts.KindSchema {
		return facts.Proposal{}, invalid("subject_kind %q is not index, table or schema",
			req.Kind)
	}
	value := map[string]string{}
	for k, v := range map[string]string{"repo": req.Repo, "path": req.Path} {
		if v = strings.TrimSpace(v); v != "" {
			value[k] = v
		}
	}
	switch req.Mark {
	case MarkOwned:
	case MarkExempt:
		value["note"] = clipBytes("exempt: "+evidence, 300)
	default:
		return facts.Proposal{}, invalid("mark %q is not owned or exempt", req.Mark)
	}
	return facts.Proposal{Type: facts.TypeAppMigrations, Kind: kind, Subject: req.Subject,
		Value: value, Source: facts.SourceOperator, ProposedBy: actor, Rationale: evidence,
		Evidence: []facts.Citation{{Kind: "agent_mark", Ref: actor, Detail: evidence,
			ObservedAt: now()}}}, nil
}

const objectKindSQL = `/* pg_sage */ SELECT c.relkind::text FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`

// requireObject checks that an exact index or table subject exists with
// that kind; patterns and schemas name objects that may come later.
func (t *Tools) requireObject(ctx context.Context, p facts.Pattern) error {
	if p.Kind == facts.KindSchema || strings.Contains(p.Schema+p.Name, "*") {
		return nil
	}
	var relkind string
	err := t.pool.QueryRow(ctx, objectKindSQL, p.Schema, p.Name).Scan(&relkind)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: no %s %s in this database", ErrNotFound, p.Kind, p.String())
	}
	if err != nil {
		return fmt.Errorf("look up %s %s: %w", p.Kind, p.String(), err)
	}
	want := map[facts.Kind]string{facts.KindIndex: "iI", facts.KindTable: "rpfvm"}[p.Kind]
	if !strings.Contains(want, relkind) {
		return fmt.Errorf("%w: %s is not a %s (relkind %s)", ErrNotFound, p.String(), p.Kind,
			relkind)
	}
	return nil
}
