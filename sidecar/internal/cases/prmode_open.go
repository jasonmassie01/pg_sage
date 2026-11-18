package cases

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/codehost"
)

// prRef is "<repo>#<n>".
func prRef(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

// covered reports whether a PR already covers c: a source-fix row on its
// finding (a declined one is never proposed again), or an open PR with
// c's marker, which is recorded again when its row was lost.
func (m *PRMode) covered(ctx context.Context, c PRCandidate, key string,
) (PROutcome, bool, error) {
	row, found, err := m.deps.Store.SourceFix(ctx, c.FindingID)
	if err != nil {
		return PROutcome{}, true, fmt.Errorf("read source fix of finding %d: %w",
			c.FindingID, err)
	}
	if found {
		return coveredByRow(row), true, nil
	}
	pr, found, err := m.findMarked(ctx, key)
	if err != nil {
		return refusalDone(err)
	}
	if !found {
		return PROutcome{}, false, nil
	}
	out := PROutcome{Status: PRCovered, Repo: m.cfg.Repo, Number: pr.Number, URL: pr.URL,
		Note: "open as " + prRef(m.cfg.Repo, pr.Number)}
	if err := m.deps.Recorder.RecordOpened(ctx, c.FindingID, m.cfg.Repo, pr); err != nil {
		out.Note += "; recording it failed and is retried next cycle"
		return out, true, fmt.Errorf("record %s for finding %d: %w",
			prRef(m.cfg.Repo, pr.Number), c.FindingID, err)
	}
	return out, true, nil
}

func refusalDone(err error) (PROutcome, bool, error) {
	out, err := refusal(err)
	return out, true, err
}

func coveredByRow(row SourceFixPR) PROutcome {
	out := PROutcome{Status: PRCovered, Repo: row.Repo, Number: row.Number, URL: row.URL}
	switch {
	case row.Origin == originPGSage && row.PRState == "closed":
		out.Status, out.Note = PRDeclined, "declined in "+prRef(row.Repo, row.Number)
	case row.Origin == originPGSage:
		out.Note = "covered by " + prRef(row.Repo, row.Number)
	default:
		out.Note = "covered by a coding agent's PR " + row.URL
	}
	return out
}

const originPGSage = "pg_sage"

// marker is the hidden line that identifies a PR's candidate.
func prMarker(key string) string { return "<!-- pg_sage:key=" + key + " -->" }

// findMarked searches the open PRs carrying the label for key's marker.
func (m *PRMode) findMarked(ctx context.Context, key string,
) (codehost.PullRequest, bool, error) {
	prs, err := m.deps.Host.ListOpenPRs(ctx, m.cfg.Repo, m.cfg.Label)
	if err != nil {
		return codehost.PullRequest{}, false, fmt.Errorf("list open PRs of %s: %w",
			m.cfg.Repo, err)
	}
	marker := prMarker(key)
	for _, pr := range prs {
		if strings.Contains(pr.Body, marker) {
			return pr, true, nil
		}
	}
	return codehost.PullRequest{}, false, nil
}

// open builds the change, opens the PR and records it. A refused open
// records nothing; a failed record is retried next cycle by the marker.
func (m *PRMode) open(ctx context.Context, c PRCandidate) (PROutcome, error) {
	reason, err := m.deps.Recorder.CanRecord(ctx, c.FindingID)
	if err != nil {
		return refusal(fmt.Errorf("check the source-fix packet of finding %d: %w",
			c.FindingID, err))
	}
	if reason != "" {
		return notOpened(reason), nil
	}
	change, err := m.change(ctx, c)
	if err != nil {
		return refusal(err)
	}
	key := c.Key()
	req := codehost.PRRequest{Title: prTitle(c.Title), Branch: "pg_sage/" + key,
		Base: m.cfg.Base, CommitMessage: prTitle(c.Title) + "\n\n" + prMarker(key),
		Labels: []string{m.cfg.Label}, Files: change.files,
		Body: m.description(ctx, c, change, key)}
	pr, openErr := m.deps.Host.OpenPR(ctx, m.cfg.Repo, req)
	if openErr != nil && pr.Number <= 0 {
		return refusal(openErr) // the host's error names the host
	}
	ref := prRef(m.cfg.Repo, pr.Number)
	out := PROutcome{Status: PROpened, Repo: m.cfg.Repo, Number: pr.Number, URL: pr.URL,
		Note: "opened as " + ref}
	if openErr != nil { // opened, then a later step failed (labels): record it anyway
		out.Note += "; " + openErr.Error()
	}
	if err := m.deps.Recorder.RecordOpened(ctx, c.FindingID, m.cfg.Repo, pr); err != nil {
		out.Note += "; recording it failed and is retried next cycle"
		return out, fmt.Errorf("record %s for finding %d: %w", ref, c.FindingID, err)
	}
	return out, openErr
}

// prTitle is the PR's title, at most 200 characters.
func prTitle(title string) string {
	t := "pg_sage: " + strings.Join(strings.Fields(title), " ")
	if r := []rune(t); len(r) > 200 {
		t = string(r[:199]) + "…"
	}
	return t
}

// prChange is the PR's files and where it touches the application.
type prChange struct {
	files    []codehost.FileChange
	callSite string
}

func (m *PRMode) change(ctx context.Context, c PRCandidate) (prChange, error) {
	if c.Kind == PRKindRewrite {
		return m.rewriteChange(ctx, c)
	}
	files, err := m.migrationChange(ctx, c.Title, c.MigrationSQL, c.RollbackSQL)
	return prChange{files: files}, err
}

// migrationChange names the new migration file(s) after the newest one.
func (m *PRMode) migrationChange(ctx context.Context, title, up, down string,
) ([]codehost.FileChange, error) {
	names, err := m.deps.Host.ListDir(ctx, m.cfg.Repo, m.cfg.MigrationsPath)
	if err != nil {
		return nil, fmt.Errorf("list %s in %s: %w", m.cfg.MigrationsPath, m.cfg.Repo, err)
	}
	next, err := nextMigrationNames(names, title, m.deps.Now())
	if err != nil {
		return nil, fmt.Errorf("%w: %s in %s: %v", errNoPR, m.cfg.MigrationsPath,
			m.cfg.Repo, strings.TrimPrefix(err.Error(), errNoPR.Error()+": "))
	}
	return migrationFiles(m.cfg.MigrationsPath, next, up, down), nil
}

// rewriteChange edits the statement's literal at an exact call site.
func (m *PRMode) rewriteChange(ctx context.Context, c PRCandidate) (prChange, error) {
	const keep = "; the rewrite stays as text for a person or coding agent"
	switch {
	case !c.RewriteChecked:
		return prChange{}, fmt.Errorf("%w: no equivalence check passed for the rewrite%s",
			errNoPR, keep)
	case m.deps.CallSites == nil:
		return prChange{}, fmt.Errorf("%w: no call-site lookup is configured%s", errNoPR, keep)
	case c.QueryID == 0:
		return prChange{}, fmt.Errorf("%w: the rewrite names no queryid%s", errNoPR, keep)
	}
	site, err := m.deps.CallSites.CallSite(ctx, c.QueryID)
	if err != nil {
		return prChange{}, fmt.Errorf("call-site lookup failed: %w", err)
	}
	if site.Confidence != "exact" {
		note := ""
		if site.Note != "" {
			note = " (" + site.Note + ")"
		}
		return prChange{}, fmt.Errorf("%w: the call site is %s, not exact%s%s", errNoPR,
			site.Confidence, note, keep)
	}
	file, err := m.deps.Host.GetFile(ctx, m.cfg.Repo, site.Path)
	if err != nil {
		return prChange{}, fmt.Errorf("read %s in %s: %w", site.Path, m.cfg.Repo, err)
	}
	edited, err := replaceLiteral(file.Content, site.Line, c.OriginalSQL, c.RewrittenSQL)
	if err != nil {
		return prChange{}, fmt.Errorf("%w: %s:%d: %v%s", errNoPR, site.Path, site.Line,
			strings.TrimPrefix(err.Error(), errNoPR.Error()+": "), keep)
	}
	return prChange{files: []codehost.FileChange{{Path: site.Path, Content: edited}},
		callSite: fmt.Sprintf("%s:%d (exact)", site.Path, site.Line)}, nil
}
