package cases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/verify"
)

// PR mode (pr-01b) turns a change pg_sage cannot apply itself, a schema
// migration or a query rewrite, into one pull request on the application's
// repository. pg_sage edits application code only for an exact literal or a
// new migration file; a person merges. Everything else stays advice.

// PR kinds.
const (
	PRKindMigration = "migration"
	PRKindRewrite   = "query_rewrite"
)

// Outcome statuses.
const (
	PROpened    = "opened"
	PRCovered   = "covered"
	PRNotOpened = "not_opened"
	PRDeclined  = "declined"
	PRMerged    = "merged"
	PRVerified  = "verified"
	PRReverting = "reverting"
)

const (
	// DefaultPRLabel labels every PR pg_sage opens; the marker search
	// lists the open PRs carrying it.
	DefaultPRLabel = "pg_sage"
	// MaxPRBody is GitHub's pull request body limit in characters.
	MaxPRBody = 65536
)

var (
	prLabelPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,50}$`)
	prKeyPattern   = regexp.MustCompile(`^f[0-9]+-[a-z_]+-[0-9a-f]{12}(-revert)?$`)
	// errNoPR is a candidate PR mode will not open a PR for; the case keeps
	// the change as text and says why.
	errNoPR = errors.New("PR not opened")
)

// PRModeConfig is the repository PR mode writes to.
type PRModeConfig struct {
	Repo           string
	MigrationsPath string
	Label          string
	// Base is the target branch; empty means the repository's default.
	Base string
}

func (c PRModeConfig) validate() error {
	if err := codehost.ValidateRepo(c.Repo); err != nil {
		return fmt.Errorf("pr_mode.repo: %w", err)
	}
	if err := codehost.ValidatePath(c.MigrationsPath); err != nil {
		return fmt.Errorf("pr_mode.migrations_path: %w", err)
	}
	if !prLabelPattern.MatchString(c.Label) {
		return fmt.Errorf("pr_mode.label %q must be 1 to 50 of A-Z a-z 0-9 _ . : -", c.Label)
	}
	return nil
}

// PRCandidate is one change PR mode may open a PR for.
type PRCandidate struct {
	FindingID    int64
	Database     string
	Kind         string
	Title        string
	Why          string
	EvidenceIDs  []string
	RiskTier     string
	MigrationSQL string
	RollbackSQL  string
	QueryID      int64
	OriginalSQL  string
	RewrittenSQL string
	// RewriteChecked is set only when the rewrite passed an equivalence
	// check; an unchecked rewrite never becomes a PR.
	RewriteChecked bool
	Ladder         []verify.LadderRecord
}

// Key identifies the candidate: its finding, kind and change. It is the
// PR's hidden marker and branch name.
func (c PRCandidate) Key() string {
	sum := sha256.Sum256([]byte(c.Kind + "\x00" + c.MigrationSQL + "\x00" +
		c.OriginalSQL + "\x00" + c.RewrittenSQL))
	return fmt.Sprintf("f%d-%s-%s", c.FindingID, c.Kind, hex.EncodeToString(sum[:6]))
}

// incomplete says why the candidate cannot be a PR, or "".
func (c PRCandidate) incomplete() string {
	switch {
	case c.FindingID <= 0:
		return "the change has no finding to record the PR against"
	case strings.TrimSpace(c.Title) == "":
		return "the change has no title"
	case c.Kind == PRKindMigration && strings.TrimSpace(c.MigrationSQL) == "":
		return "the migration has no SQL"
	case c.Kind == PRKindRewrite && strings.TrimSpace(c.OriginalSQL) == "":
		return "the rewrite has no original statement"
	case c.Kind != PRKindMigration && c.Kind != PRKindRewrite:
		return fmt.Sprintf("kind %q is not a migration or a query rewrite", c.Kind)
	}
	return ""
}

// PRCallSite is where the application sends a statement (pr-01a).
type PRCallSite struct {
	Path       string
	Line       int
	Confidence string // exact, likely or none
	Note       string
}

// CallSiteFinder finds a statement's call site (agenttools.Tools.CallSite).
type CallSiteFinder interface {
	CallSite(ctx context.Context, queryID int64) (PRCallSite, error)
}

// SourceFixPR is a finding's sage.source_fix row, as PR mode reads it.
type SourceFixPR struct {
	Origin, Repo, URL, Stage, PRState, Verdict string
	Number                                     int
}

// PRStore holds PR mode's locks and reads and updates source-fix rows.
type PRStore interface {
	// Lock takes the candidate key's lock; ok is false while another
	// cycle holds it. unlock releases it.
	Lock(ctx context.Context, key string) (unlock func(), ok bool, err error)
	SourceFix(ctx context.Context, findingID int64) (SourceFixPR, bool, error)
	SetPRState(ctx context.Context, findingID int64, state string) error
}

// SourceFixRecorder records an opened PR through the source-fix stage
// pr_opened (agenttools).
type SourceFixRecorder interface {
	// CanRecord says why the finding's PR could not be recorded (no
	// source-fix packet), or "" when it can.
	CanRecord(ctx context.Context, findingID int64) (reason string, err error)
	RecordOpened(ctx context.Context, findingID int64, repo string,
		pr codehost.PullRequest) error
}

// PRModel writes the PR's plain-words paragraph (internal/llm).
type PRModel interface {
	Chat(ctx context.Context, system, user string) (string, error)
}

// PRModeDeps are PR mode's collaborators; Model and CallSites are optional.
type PRModeDeps struct {
	Host      codehost.Host
	Store     PRStore
	Recorder  SourceFixRecorder
	Model     PRModel
	CallSites CallSiteFinder
	Now       func() time.Time
}

// PRMode opens and follows PRs. A nil *PRMode is off and changes nothing.
type PRMode struct {
	cfg  PRModeConfig
	deps PRModeDeps
}

// NewPRMode builds PR mode. No repository means off: nil, nil.
func NewPRMode(cfg PRModeConfig, deps PRModeDeps) (*PRMode, error) {
	if cfg.Repo == "" {
		return nil, nil
	}
	if cfg.Label == "" {
		cfg.Label = DefaultPRLabel
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if deps.Host == nil || deps.Store == nil || deps.Recorder == nil {
		return nil, errors.New("pr_mode needs a code host (agents.code_host) and the " +
			"source-fix store")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &PRMode{cfg: cfg, deps: deps}, nil
}

// PROutcome is what PR mode did with a candidate; Note is the case's line.
type PROutcome struct {
	Status string `json:"status,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	Note   string `json:"note,omitempty"`
}

func notOpened(reason string) PROutcome {
	return PROutcome{Status: PRNotOpened, Note: "PR not opened: " + reason}
}

// refusal maps errNoPR to the outcome alone and any other failure to the
// outcome plus the error.
func refusal(err error) (PROutcome, error) {
	if errors.Is(err, errNoPR) {
		return notOpened(strings.TrimPrefix(err.Error(), errNoPR.Error()+": ")), nil
	}
	return notOpened(err.Error()), err
}

// ApplyPROutcome returns c with the outcome's note on every action
// candidate; an empty outcome returns c unchanged.
func ApplyPROutcome(c Case, out PROutcome) Case {
	if out.Note == "" || len(c.ActionCandidates) == 0 {
		return c
	}
	candidates := append([]ActionCandidate(nil), c.ActionCandidates...)
	for i := range candidates {
		candidates[i].PRNote = out.Note
	}
	c.ActionCandidates = candidates
	return c
}

// Run opens one PR for c unless one covers it: an open PR carrying c's
// marker, or a source-fix row on its finding. The candidate's lock keeps
// two cycles from racing.
func (m *PRMode) Run(ctx context.Context, c PRCandidate) (PROutcome, error) {
	if m == nil {
		return PROutcome{}, nil
	}
	if reason := c.incomplete(); reason != "" {
		return notOpened(reason), nil
	}
	return m.locked(ctx, c.Key(), func() (PROutcome, error) {
		if out, done, err := m.covered(ctx, c, c.Key()); done || err != nil {
			return out, err
		}
		return m.open(ctx, c)
	})
}

// locked runs fn under key's lock; a held lock is covered by its holder.
func (m *PRMode) locked(ctx context.Context, key string, fn func() (PROutcome, error),
) (PROutcome, error) {
	unlock, ok, err := m.deps.Store.Lock(ctx, key)
	if err != nil {
		return PROutcome{}, fmt.Errorf("lock PR candidate %s: %w", key, err)
	}
	if !ok {
		return PROutcome{Status: PRCovered, Note: "another cycle is opening this PR"}, nil
	}
	defer unlock()
	return fn()
}
