package agenttools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Report stages.
const (
	StagePROpened = "pr_opened"
	StageDeployed = "deployed"
	StageStatus   = "status"
	stageVerified = "verified"
)

// Bounds of a report.
const (
	maxPRURL        = 2048
	maxDeployAge    = 7 * 24 * time.Hour
	deployClockSkew = 30 * time.Second
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// ReportRequest is a coding agent's report on a finding's source fix:
// the pull request (pr_opened), the deploy (deployed), or a request for
// pg_sage's verdict (status).
type ReportRequest struct {
	FindingID  int64      `json:"finding_id"`
	Stage      string     `json:"stage"`
	PRURL      string     `json:"pr_url,omitempty"`
	Commit     string     `json:"commit,omitempty"`
	DeployedAt *time.Time `json:"deployed_at,omitempty"`
	PacketHash string     `json:"packet_hash,omitempty"`
}

// Report is where a source fix stands and, once decided, pg_sage's verdict:
// observed against predicted change of the target queries. Verdict is
// "pending" until decided; a decided verdict never changes.
// DeployReportedAt is when pg_sage first heard of the deploy (unset for reports
// filed before it was recorded); Backdated means the deploy was reported
// after its whole verify window had closed.
type Report struct {
	FindingID         int64             `json:"finding_id"`
	Stage             string            `json:"stage"`
	PRURL             string            `json:"pr_url,omitempty"`
	Commit            string            `json:"commit,omitempty"`
	DeployedAt        *time.Time        `json:"deployed_at,omitempty"`
	DeployReportedAt  *time.Time        `json:"deploy_reported_at,omitempty"`
	Backdated         bool              `json:"backdated"`
	VerifyAfter       *time.Time        `json:"verify_after,omitempty"`
	Verdict           string            `json:"verdict"`
	Tolerance         string            `json:"tolerance,omitempty"`
	ObservedChangePct *float64          `json:"observed_change_pct,omitempty"`
	Prediction        verify.Prediction `json:"prediction"`
	Evidence          map[string]any    `json:"evidence,omitempty"`
	Reason            string            `json:"reason,omitempty"`
}

// ReportSourceFix records a pull request or deploy, or returns the
// verdict (deciding it once the verify window after the deploy closed).
func (t *Tools) ReportSourceFix(ctx context.Context, req ReportRequest, actor string,
) (Report, error) {
	if err := t.ready(); err != nil {
		return Report{}, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || len(actor) > maxActor {
		return Report{}, invalid("an actor of 1..%d bytes is required", maxActor)
	}
	if req.FindingID <= 0 {
		return Report{}, invalid("finding_id %d is not positive", req.FindingID)
	}
	switch req.Stage {
	case StagePROpened:
		return t.reportPROpened(ctx, req, actor)
	case StageDeployed:
		return t.reportDeployed(ctx, req, actor)
	case StageStatus:
		return t.reportStatus(ctx, req.FindingID)
	}
	return Report{}, invalid("stage %q is not pr_opened, deployed or status", req.Stage)
}

func validPRURL(raw string) error {
	if raw == "" || len(raw) > maxPRURL {
		return invalid("pr_url must be 1..%d bytes", maxPRURL)
	}
	if strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return invalid("pr_url contains whitespace or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return invalid("pr_url must be an https URL with a host")
	}
	return nil
}

func validCommit(c string) error {
	if c != "" && !commitPattern.MatchString(c) {
		return invalid("commit must be 7..64 lower-case hex characters")
	}
	return nil
}

func (t *Tools) validDeployedAt(at *time.Time) (time.Time, error) {
	now := t.now()
	if at == nil {
		return now, nil
	}
	switch {
	case at.After(now.Add(deployClockSkew)):
		return time.Time{}, invalid("deployed_at %s is in the future", at.Format(time.RFC3339))
	case now.Sub(*at) > maxDeployAge:
		return time.Time{}, invalid("deployed_at %s is older than 7 days",
			at.Format(time.RFC3339))
	}
	return *at, nil
}

// reportPacket is the finding's current packet; a given hash must match it.
func (t *Tools) reportPacket(ctx context.Context, req ReportRequest) (Packet, error) {
	p, err := t.SourceFixPacket(ctx, req.FindingID)
	if err != nil {
		return Packet{}, err
	}
	if req.PacketHash != "" && req.PacketHash != p.Hash {
		return Packet{}, invalid("packet_hash %q is not the finding's current packet (%s): "+
			"get_source_fix_packet again", req.PacketHash, p.Hash)
	}
	return p, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

const reportColumns = `finding_id, stage, pr_url, commit_sha, deployed_at, prediction,
    target_queryids, verdict, tolerance, observed, evidence, reason, deploy_reported_at`

// reportRow is a sage.source_fix row.
type reportRow struct {
	FindingID          int64
	Stage              string
	PRURL, Commit      *string
	DeployedAt         *time.Time
	Prediction         []byte
	Targets            []int64
	Verdict, Tolerance *string
	Observed, Evidence []byte
	Reason             string
	DeployReportedAt   *time.Time
}

func scanReport(row pgx.Row) (reportRow, error) {
	var r reportRow
	err := row.Scan(&r.FindingID, &r.Stage, &r.PRURL, &r.Commit, &r.DeployedAt,
		&r.Prediction, &r.Targets, &r.Verdict, &r.Tolerance, &r.Observed, &r.Evidence,
		&r.Reason, &r.DeployReportedAt)
	return r, err
}

// prOpenedSQL records a PR: $8..$11 are its origin (agent or pg_sage, pr-01b),
// repository, number and state, null for an agent's report.
const prOpenedSQL = `/* pg_sage */ INSERT INTO sage.source_fix AS s (finding_id, packet_hash,
    stage, pr_url, commit_sha, reported_by, prediction, target_queryids, origin, repo,
    pr_number, pr_state)
VALUES ($1, $2, 'pr_opened', $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (finding_id) DO UPDATE SET pr_url = EXCLUDED.pr_url,
    origin = EXCLUDED.origin, repo = EXCLUDED.repo, pr_number = EXCLUDED.pr_number,
    pr_state = EXCLUDED.pr_state,
    commit_sha = COALESCE(EXCLUDED.commit_sha, s.commit_sha),
    packet_hash = EXCLUDED.packet_hash, reported_by = EXCLUDED.reported_by,
    prediction = EXCLUDED.prediction, target_queryids = EXCLUDED.target_queryids,
    updated_at = now()
WHERE s.stage = 'pr_opened'
RETURNING ` + reportColumns

func (t *Tools) reportPROpened(ctx context.Context, req ReportRequest, actor string,
) (Report, error) {
	if err := validPRURL(req.PRURL); err != nil {
		return Report{}, err
	}
	if err := validCommit(req.Commit); err != nil {
		return Report{}, err
	}
	return t.writePROpened(ctx, req, actor, prOrigin{name: originAgent})
}

// prOrigin is who opened a recorded PR; repo, number and state are set
// only for PR mode's own PRs.
type prOrigin struct {
	name, repo, state string
	number            int
}

func (o prOrigin) args() []any {
	if o.name == originAgent {
		return []any{o.name, nil, nil, nil}
	}
	return []any{o.name, o.repo, o.number, o.state}
}

func (t *Tools) writePROpened(ctx context.Context, req ReportRequest, actor string,
	origin prOrigin) (Report, error) {
	p, err := t.reportPacket(ctx, req)
	if err != nil {
		return Report{}, err
	}
	pred, err := json.Marshal(p.Verification.Prediction)
	if err != nil {
		return Report{}, fmt.Errorf("encode prediction: %w", err)
	}
	args := append([]any{req.FindingID, p.Hash, req.PRURL, nullable(req.Commit), actor, pred,
		toInt64s(p.Targets.QueryIDs)}, origin.args()...)
	row, err := scanReport(t.pool.QueryRow(ctx, prOpenedSQL, args...))
	return t.writtenReport(row, err, req)
}

// deployedSQL records a deploy and when it was reported ($9, the tools'
// clock, the same clock deployed_at is bounded by). Re-reporting the same
// deploy (same deployed_at) keeps the time pg_sage first heard of it, so a
// live deploy re-reported late is never marked backdated.
const deployedSQL = `/* pg_sage */ INSERT INTO sage.source_fix AS s (finding_id, packet_hash,
    stage, pr_url, commit_sha, reported_by, prediction, target_queryids, deployed_at,
    deploy_reported_at)
VALUES ($1, $2, 'deployed', $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (finding_id) DO UPDATE SET stage = 'deployed',
    deployed_at = EXCLUDED.deployed_at,
    deploy_reported_at = CASE WHEN s.deployed_at = EXCLUDED.deployed_at
        THEN s.deploy_reported_at ELSE EXCLUDED.deploy_reported_at END,
    pr_url = COALESCE(EXCLUDED.pr_url, s.pr_url),
    commit_sha = COALESCE(EXCLUDED.commit_sha, s.commit_sha),
    packet_hash = EXCLUDED.packet_hash, reported_by = EXCLUDED.reported_by,
    prediction = EXCLUDED.prediction, target_queryids = EXCLUDED.target_queryids,
    updated_at = now()
WHERE s.decided_at IS NULL
RETURNING ` + reportColumns

func (t *Tools) reportDeployed(ctx context.Context, req ReportRequest, actor string,
) (Report, error) {
	deployed, err := t.validDeployedAt(req.DeployedAt)
	if err != nil {
		return Report{}, err
	}
	if req.PRURL != "" {
		if err := validPRURL(req.PRURL); err != nil {
			return Report{}, err
		}
	}
	if err := validCommit(req.Commit); err != nil {
		return Report{}, err
	}
	p, err := t.reportPacket(ctx, req)
	if err != nil {
		return Report{}, err
	}
	pred, err := json.Marshal(p.Verification.Prediction)
	if err != nil {
		return Report{}, fmt.Errorf("encode prediction: %w", err)
	}
	row, err := scanReport(t.pool.QueryRow(ctx, deployedSQL, req.FindingID, p.Hash,
		nullable(req.PRURL), nullable(req.Commit), actor, pred, toInt64s(p.Targets.QueryIDs),
		deployed, t.now()))
	return t.writtenReport(row, err, req)
}

// writtenReport maps an upsert that changed nothing (the report is past
// that stage) to ErrTransition.
func (t *Tools) writtenReport(row reportRow, err error, req ReportRequest) (Report, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, fmt.Errorf("%w: finding %d's report is past %s", ErrTransition,
			req.FindingID, req.Stage)
	}
	if err != nil {
		return Report{}, fmt.Errorf("record %s report of finding %d: %w", req.Stage,
			req.FindingID, err)
	}
	return t.toReport(row)
}

// toReport renders a row; VerifyAfter is the deploy plus the window.
func (t *Tools) toReport(r reportRow) (Report, error) {
	out := Report{FindingID: r.FindingID, Stage: r.Stage, DeployedAt: r.DeployedAt,
		DeployReportedAt: r.DeployReportedAt, Verdict: verify.OutcomePending, Reason: r.Reason,
		Backdated: backdated(r.DeployedAt, r.DeployReportedAt, t.opts.VerifyWindow)}
	if r.PRURL != nil {
		out.PRURL = *r.PRURL
	}
	if r.Commit != nil {
		out.Commit = *r.Commit
	}
	if r.DeployedAt != nil {
		after := r.DeployedAt.Add(t.opts.VerifyWindow)
		out.VerifyAfter = &after
	}
	if r.Verdict != nil {
		out.Verdict = *r.Verdict
	}
	if r.Tolerance != nil {
		out.Tolerance = *r.Tolerance
	}
	if err := decodeReportJSON(r, &out); err != nil {
		return Report{}, fmt.Errorf("decode report of finding %d: %w", r.FindingID, err)
	}
	return out, nil
}

func decodeReportJSON(r reportRow, out *Report) error {
	if err := json.Unmarshal(r.Prediction, &out.Prediction); err != nil {
		return fmt.Errorf("prediction: %w", err)
	}
	if len(r.Observed) > 0 {
		var obs verify.Observed
		if err := json.Unmarshal(r.Observed, &obs); err != nil {
			return fmt.Errorf("observed: %w", err)
		}
		out.ObservedChangePct = obs.ChangePct
	}
	if len(r.Evidence) > 0 {
		if err := json.Unmarshal(r.Evidence, &out.Evidence); err != nil {
			return fmt.Errorf("evidence: %w", err)
		}
	}
	return nil
}

// backdated reports a deploy heard of only after its whole verify window
// (the after-window) had closed. An unknown report time is not backdated.
func backdated(deployed, reported *time.Time, window time.Duration) bool {
	return deployed != nil && reported != nil && reported.Sub(*deployed) > window
}
