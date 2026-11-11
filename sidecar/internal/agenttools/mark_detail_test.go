package agenttools

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/facts"
)

// facts-09 review: a mark's repo and path live in its citation detail,
// which the fact store cuts at 500 bytes from the end. Long evidence must
// not push them out; the evidence is shortened instead (the rationale
// keeps it whole).
func TestMarkDetailKeepsRepoAndPathWithLongEvidence(t *testing.T) {
	evidence := strings.Repeat("e", 500)
	req := MarkRequest{Kind: "schema", Subject: "app", Mark: MarkOwned,
		Repo: "github.com/acme/app", Path: "db/migrate", Evidence: evidence}
	p, err := markProposal(req, "agent:claude-code", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := facts.Validate(p)
	if err != nil {
		t.Fatal(err)
	}
	detail := got.Evidence[0].Detail
	if !strings.HasSuffix(detail, " (repo github.com/acme/app, path db/migrate)") ||
		len(detail) != markDetailLimit || !strings.HasPrefix(detail, "eeee") {
		t.Fatalf("detail (%d bytes) = %q", len(detail), detail)
	}
	if got.Rationale != evidence {
		t.Fatalf("rationale lost the full evidence: %d bytes", len(got.Rationale))
	}
}

// Boundary: evidence that fits with the location is kept whole, and a cut
// never splits a multi-byte character.
func TestMarkDetailBoundaries(t *testing.T) {
	suffix := " (repo r)"
	fits := strings.Repeat("a", markDetailLimit-len(suffix))
	if got := markDetail(fits, "r", ""); got != fits+suffix {
		t.Fatalf("evidence that fits was cut: %d bytes", len(got))
	}
	over := strings.Repeat("é", markDetailLimit) // 2 bytes each
	got := markDetail(over, "r", "")
	if !strings.HasSuffix(got, suffix) || len(got) > markDetailLimit ||
		!strings.HasPrefix(got, "é") || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("multi-byte cut (%d bytes) = %q", len(got), got[len(got)-20:])
	}
	if got := markDetail("why", " ", "\t"); got != "why" {
		t.Fatalf("blank repo and path: %q", got)
	}
}
