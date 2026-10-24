package impact

import (
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
)

// In-file transaction opt-outs. A migration tool that runs each file in a
// transaction lets one file opt out with a directive; the opt-out holds
// when the transaction comes from the tool's default (a request that says
// transactional wins: it knows how it runs the file).
var (
	gooseNoTx      = regexp.MustCompile(`(?i)^\s*--\s*\+goose\s+no\s+transaction\b`)
	dbmateSection  = regexp.MustCompile(`(?i)^\s*--\s*migrate:(up|down)\b(.*)$`)
	liquibaseSet   = regexp.MustCompile(`(?i)^\s*--\s*changeset\s+\S+(.*)$`)
	atlasNoTx      = regexp.MustCompile(`(?i)^\s*--\s*atlas:txmode\s+none\b`)
	dbmateNoTx     = regexp.MustCompile(`(?i)\btransaction:\s*false\b`)
	liquibaseNoTx  = regexp.MustCompile(`(?i)\brunInTransaction:\s*false\b`)
	sectionedTools = map[string]*regexp.Regexp{"dbmate": dbmateSection,
		"liquibase": liquibaseSet}
	sectionNoTx = map[string]*regexp.Regexp{"dbmate": dbmateNoTx,
		"liquibase": liquibaseNoTx}
)

// txnOptOut reports, by 1-based line of content, whether the migration
// tool runs that line outside a transaction because the file says so:
// goose's "-- +goose NO TRANSACTION" and atlas's "-- atlas:txmode none"
// for the whole file, dbmate's "-- migrate:up transaction:false" and
// liquibase's "--changeset ... runInTransaction:false" for their section.
func txnOptOut(tool, content string) func(line int) bool {
	lines := strings.Split(content, "\n")
	switch tool {
	case "goose", "atlas":
		re := gooseNoTx
		if tool == "atlas" {
			re = atlasNoTx
		}
		for _, l := range lines {
			if re.MatchString(l) {
				return func(int) bool { return true }
			}
		}
	case "dbmate", "liquibase":
		return sectionOptOut(lines, sectionedTools[tool], sectionNoTx[tool])
	}
	return func(int) bool { return false }
}

// sectionOptOut marks the lines of each section whose header opts out.
func sectionOptOut(lines []string, header, noTx *regexp.Regexp) func(int) bool {
	off := make([]bool, len(lines)+1)
	cur := false
	for i, l := range lines {
		if m := header.FindStringSubmatch(l); m != nil {
			cur = noTx.MatchString(m[len(m)-1])
		}
		off[i+1] = cur
	}
	return func(line int) bool { return line > 0 && line < len(off) && off[line] }
}

// transactional reports whether s runs in a transaction its file does
// not open itself: the request's or the tool's, unless the file opts out
// of the tool's default.
func (r *run) transactional(s *stmt) bool {
	if !r.in.Transactional {
		return false
	}
	if r.in.TransactionalFrom == changereview.TransactionalFromRequest {
		return true
	}
	return !s.noTxn
}
