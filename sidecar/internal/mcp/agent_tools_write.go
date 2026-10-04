package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/pg-sage/sidecar/internal/agenttools"
)

func markCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args struct {
		Database    string `json:"database"`
		SubjectKind string `json:"subject_kind"`
		Subject     string `json:"subject"`
		Mark        string `json:"mark"`
		Repo        string `json:"repo"`
		Path        string `json:"path"`
		Evidence    string `json:"evidence"`
	}
	switch {
	case !decodeStrict(raw, &args):
		return nil, invalid("mark_object takes subject_kind, subject, mark, repo, path " +
			"and evidence")
	case args.SubjectKind != "index" && args.SubjectKind != "table" &&
		args.SubjectKind != "schema":
		return nil, invalid("subject_kind is index, table or schema")
	case args.Mark != "owned" && args.Mark != "exempt":
		return nil, invalid("mark is owned or exempt")
	case !boundedText(args.Subject, 1, 300) || !boundedText(args.Evidence, 1, 500) ||
		!boundedText(args.Repo, 0, 300) || !boundedText(args.Path, 0, 300):
		return nil, invalid("subject (1-300), evidence (1-500), repo and path (0-300) " +
			"must be plain text")
	}
	request := agenttools.MarkRequest{Kind: args.SubjectKind, Subject: args.Subject,
		Mark: args.Mark, Repo: args.Repo, Path: args.Path, Evidence: args.Evidence}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.MarkObject(ctx, request, ActorFromContext(ctx))
	}, nil
}

// boundedText is trimmed text of min..max bytes with no control characters.
func boundedText(s string, minLen, maxLen int) bool {
	trimmed := strings.TrimSpace(s)
	return len(trimmed) >= minLen && len(s) <= maxLen &&
		strings.IndexFunc(s, unicode.IsControl) < 0
}

func packetCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args struct {
		Database  string `json:"database"`
		FindingID int64  `json:"finding_id"`
	}
	if !decodeStrict(raw, &args) || args.FindingID < 1 {
		return nil, invalid("get_source_fix_packet takes a finding_id >= 1")
	}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.SourceFixPacket(ctx, args.FindingID)
	}, nil
}

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

type reportArgs struct {
	Database   string `json:"database"`
	FindingID  int64  `json:"finding_id"`
	Stage      string `json:"stage"`
	PRURL      string `json:"pr_url"`
	Commit     string `json:"commit"`
	DeployedAt string `json:"deployed_at"`
	PacketHash string `json:"packet_hash"`
}

func reportCall(raw json.RawMessage) (agentInvocation, *rpcError) {
	var args reportArgs
	if !decodeStrict(raw, &args) || args.FindingID < 1 {
		return nil, invalid("report_source_fix takes finding_id, stage, pr_url, commit, " +
			"deployed_at and packet_hash")
	}
	request, failed := args.request()
	if failed != nil {
		return nil, failed
	}
	return func(ctx context.Context, b AgentToolBackend) (any, error) {
		return b.ReportSourceFix(ctx, request, ActorFromContext(ctx))
	}, nil
}

func (a reportArgs) request() (agenttools.ReportRequest, *rpcError) {
	request := agenttools.ReportRequest{FindingID: a.FindingID, Stage: a.Stage,
		PRURL: a.PRURL, Commit: a.Commit, PacketHash: a.PacketHash}
	switch {
	case a.Stage != "pr_opened" && a.Stage != "deployed" && a.Stage != "status":
		return request, invalid("stage is pr_opened, deployed or status")
	case a.PRURL != "" && !httpsURL(a.PRURL):
		return request, invalid("pr_url must be an https URL (at most 500 characters)")
	case a.Commit != "" && !commitPattern.MatchString(a.Commit):
		return request, invalid("commit is a lowercase hex SHA (7-64 characters)")
	case !boundedText(a.PacketHash, 0, 128):
		return request, invalid("packet_hash is at most 128 characters")
	}
	if a.DeployedAt != "" {
		at, err := time.Parse(time.RFC3339, a.DeployedAt)
		if err != nil {
			return request, invalid("deployed_at must be an RFC 3339 date-time")
		}
		request.DeployedAt = &at
	}
	return request, nil
}

func httpsURL(raw string) bool {
	if len(raw) > 500 || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" &&
		parsed.User == nil
}

// packetResult renders a source-fix packet for the model (content) and
// keeps the typed packet for clients (structuredContent).
func packetResult(packet agenttools.Packet) map[string]any {
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": RenderPacket(packet)}},
		"structuredContent": packet,
	}
}
