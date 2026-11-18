package cases

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// Follow moves PR mode's PR for c along: open, closed unmerged (declined),
// merged, deployed (the source-fix report), then the verdict (mcp-06's
// verify). A regressed migration that has a rollback gets one revert PR.
// A PR a coding agent reported is the agent's to follow: zero outcome.
func (m *PRMode) Follow(ctx context.Context, c PRCandidate) (PROutcome, error) {
	if m == nil || c.FindingID <= 0 {
		return PROutcome{}, nil
	}
	row, found, err := m.deps.Store.SourceFix(ctx, c.FindingID)
	if err != nil {
		return PROutcome{}, fmt.Errorf("read source fix of finding %d: %w", c.FindingID, err)
	}
	if !found || row.Origin != originPGSage {
		return PROutcome{}, nil
	}
	ref := prRef(row.Repo, row.Number)
	out := PROutcome{Repo: row.Repo, Number: row.Number, URL: row.URL}
	switch {
	case row.Verdict == "regressed":
		return m.regressed(ctx, c, row)
	case row.Verdict != "":
		out.Status, out.Note = PRVerified, "verified "+row.Verdict+" after "+ref
	case row.PRState == "closed":
		out.Status, out.Note = PRDeclined, "declined in "+ref
	case row.Stage == "deployed":
		out.Status = PRMerged
		out.Note = "deployed; pg_sage is measuring the effect of " + ref
	case row.PRState == "merged":
		out.Status = PRMerged
		out.Note = "merged in " + ref + "; pg_sage measures the effect once the deploy " +
			"is reported"
	default:
		return m.followOpen(ctx, c.FindingID, row)
	}
	return out, nil
}

// followOpen asks the host for an open PR's state and records a change.
func (m *PRMode) followOpen(ctx context.Context, findingID int64, row SourceFixPR,
) (PROutcome, error) {
	ref := prRef(row.Repo, row.Number)
	out := PROutcome{Status: PROpened, Repo: row.Repo, Number: row.Number, URL: row.URL,
		Note: "open as " + ref}
	st, err := m.deps.Host.PRState(ctx, row.Repo, row.Number)
	if err != nil {
		return out, fmt.Errorf("read the state of %s: %w", ref, err)
	}
	switch st.State {
	case codehost.PRClosed:
		out.Status, out.Note = PRDeclined, "declined in "+ref
	case codehost.PRMerged:
		out.Status = PRMerged
		out.Note = "merged in " + ref + "; pg_sage measures the effect once the deploy " +
			"is reported"
	default:
		return out, nil
	}
	if err := m.deps.Store.SetPRState(ctx, findingID, string(st.State)); err != nil {
		return out, fmt.Errorf("record %s as %s: %w", ref, st.State, err)
	}
	return out, nil
}

// regressed files the revert PR of a regressed migration, once: the
// rollback as a new migration. A rewrite or a forward-only migration is
// left to a person.
func (m *PRMode) regressed(ctx context.Context, c PRCandidate, row SourceFixPR,
) (PROutcome, error) {
	ref := prRef(row.Repo, row.Number)
	out := PROutcome{Status: PRVerified, Repo: row.Repo, Number: row.Number, URL: row.URL,
		Note: "regressed after " + ref}
	switch {
	case row.PRState == "revert_opened":
		out.Status, out.Note = PRReverting, out.Note+"; revert opened"
		return out, nil
	case c.Kind == PRKindRewrite:
		out.Note += "; revert the application change by reverting " + ref
		return out, nil
	case strings.TrimSpace(c.RollbackSQL) == "":
		out.Note += "; no rollback to revert with: the migration is a forward fix only"
		return out, nil
	}
	return m.locked(ctx, c.Key()+"-revert", func() (PROutcome, error) {
		return m.openRevert(ctx, c, ref, out)
	})
}

func (m *PRMode) openRevert(ctx context.Context, c PRCandidate, ref string, out PROutcome,
) (PROutcome, error) {
	key := c.Key() + "-revert"
	pr, found, err := m.findMarked(ctx, key)
	if err != nil {
		return out, err
	}
	if !found {
		if pr, err = m.openRevertPR(ctx, c, ref, key); err != nil {
			out.Note += "; revert PR not opened: " + err.Error()
			return out, err
		}
	}
	out.Status = PRReverting
	out.Note += "; revert opened as " + prRef(m.cfg.Repo, pr.Number)
	if err := m.deps.Store.SetPRState(ctx, c.FindingID, "revert_opened"); err != nil {
		return out, fmt.Errorf("record the revert of %s: %w", ref, err)
	}
	return out, nil
}

func (m *PRMode) openRevertPR(ctx context.Context, c PRCandidate, ref, key string,
) (codehost.PullRequest, error) {
	title := "Revert: " + c.Title
	files, err := m.migrationChange(ctx, title, c.RollbackSQL, c.MigrationSQL)
	if err != nil {
		return codehost.PullRequest{}, err
	}
	sections := []prSection{
		{"What this changes", "This adds the rollback of " + ref + " as a new migration, " +
			files[0].Path + ". pg_sage does not run it; a person merges and deploys it."},
		{"Why", fmt.Sprintf("After %s was deployed, pg_sage measured the target queries "+
			"and the verdict was regressed.\n\nEvidence: finding:%d.", ref, c.FindingID)},
		{"Risk and rollback", "Risk tier: " + c.RiskTier + ". To undo this revert, " +
			"apply the change of " + ref + " again."},
	}
	return m.deps.Host.OpenPR(ctx, m.cfg.Repo, codehost.PRRequest{Title: prTitle(title),
		Branch: "pg_sage/" + key, Base: m.cfg.Base,
		CommitMessage: prTitle(title) + "\n\n" + prMarker(key),
		Labels:        []string{m.cfg.Label}, Files: files,
		Body: renderPRBody(sections, prMarker(key), MaxPRBody)})
}
