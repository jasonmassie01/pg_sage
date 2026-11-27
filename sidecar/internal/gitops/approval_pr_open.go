package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/codehost"
)

// Opening approval PRs (cli-02b): one per change, claimed in the table
// first so no change ever gets a second PR; a failure is retried with
// backoff and a refused token turns PR mode off.

const (
	retryBase = 30 * time.Second
	retryCap  = 30 * time.Minute
)

// retryBackoff is the wait after the n-th failed open: 30 s, doubling,
// at most 30 min.
func retryBackoff(n int) time.Duration {
	d := retryBase
	for i := 1; i < n && d < retryCap; i++ {
		d *= 2
	}
	return min(d, retryCap)
}

var httpStatus = regexp.MustCompile(`HTTP (\d{3})`)

// openFailureStatus is how a failed open is shown: the HTTP status when
// the host answered one, else the failure's class. It never carries the
// error text, which could echo what the host said.
func openFailureStatus(err error) string {
	var rl *codehost.RateLimitError
	switch m := httpStatus.FindStringSubmatch(errString(err)); {
	case errors.As(err, &rl) || errors.Is(err, codehost.ErrRateLimited):
		return "rate limited"
	case m != nil:
		return "HTTP " + m[1]
	case errors.Is(err, codehost.ErrUnavailable):
		return "code host unavailable"
	case errors.Is(err, codehost.ErrInvalid):
		return "request refused"
	default:
		return "error"
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// filePath is <path><database>/<queue_id>.yaml, checked as a repo path.
func (c ApprovalPRConfig) filePath(database string, queueID int) (string, error) {
	base := strings.TrimSpace(c.Path)
	if base == "" {
		base = "approvals/"
	}
	base = strings.TrimSuffix(base, "/") + "/"
	if queueID <= 0 || database == "" || strings.ContainsAny(database, `/\`) {
		return "", fmt.Errorf("%w: no approval file path for database %q, item %d",
			codehost.ErrInvalid, database, queueID)
	}
	p := base + database + "/" + strconv.Itoa(queueID) + ".yaml"
	if err := codehost.ValidatePath(p); err != nil {
		return "", err
	}
	return p, nil
}

// ladderLine is one ladder receipt entry in the approval file.
type ladderLine struct {
	Tier   int       `yaml:"tier"`
	Name   string    `yaml:"name"`
	Result string    `yaml:"result"`
	At     time.Time `yaml:"at"`
}

// approvalFile is the file an approval PR adds.
type approvalFile struct {
	Kind          string       `yaml:"kind"`
	SchemaVersion int          `yaml:"schema_version"`
	Database      string       `yaml:"database"`
	QueueID       int          `yaml:"queue_id"`
	Title         string       `yaml:"title"`
	CardHash      string       `yaml:"card_hash"`
	RiskTier      string       `yaml:"risk_tier"`
	SQL           string       `yaml:"sql"`
	Rollback      string       `yaml:"rollback"`
	RollbackClass string       `yaml:"rollback_class,omitempty"`
	Ladder        []ladderLine `yaml:"ladder,omitempty"`
	LadderNote    string       `yaml:"ladder_note,omitempty"`
	ExpiresAt     time.Time    `yaml:"expires_at"`
}

// renderApprovalFile is the approval file of item, expiring at expires.
func renderApprovalFile(item ApprovalItem, expires time.Time) (string, error) {
	c := item.Card
	f := approvalFile{Kind: approvalFileKind, SchemaVersion: SchemaVersion,
		Database: c.Database, QueueID: c.QueueID, Title: c.Title, CardHash: c.CardHash,
		RiskTier: string(item.Tier), SQL: c.SQL, Rollback: c.Rollback.SQL,
		RollbackClass: c.Rollback.Class, LadderNote: c.LadderNote,
		ExpiresAt: expires.UTC()}
	for _, l := range c.Ladder {
		f.Ladder = append(f.Ladder, ladderLine{Tier: l.Tier, Name: l.Name,
			Result: l.Result, At: l.At.UTC()})
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("gitops: rendering the approval file of %s/%d: %w",
			c.Database, c.QueueID, err)
	}
	return string(out), nil
}

func fileHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// expiry is when the PR closes: Config.Expiry from now, or sooner when the
// change itself expires first.
func (p *ApprovalPRs) expiry(c approvalcard.Card) time.Time {
	d := p.Config.Expiry
	if d <= 0 {
		d = 72 * time.Hour
	}
	at := p.now().Add(d)
	if !c.ExpiresAt.IsZero() && c.ExpiresAt.Before(at) {
		at = c.ExpiresAt.UTC()
	}
	return at
}

// remoteAllowed is D7's cap: a tier a remote approval may not decide is
// never opened as a PR and never approved by one.
func (p *ApprovalPRs) remoteAllowed(item ApprovalItem) (bool, string) {
	return approvalcard.RemoteApprovalAllowed(item.Tier, p.Config.ChatCap)
}
