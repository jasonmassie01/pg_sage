package logwatch

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/rca"
)

// rca-07b: the deadlock DETAIL PostgreSQL writes to the server log names,
// per process, the lock it waited for and its holder, then the statement
// each process ran (DeadLockReport, English lc_messages):
//
//	Process 4242 waits for ShareLock on transaction 745; blocked by process 4243.
//	Process 4243 waits for ShareLock on transaction 744; blocked by process 4242.
//	Process 4242: UPDATE ...
//	Process 4243: UPDATE ...
//
// The first wait line is the detecting backend's, the one that logs the
// error, so the relation its CONTEXT names belongs to that participant.
// Statements are never kept raw: they are clipped, literal-redacted and
// fingerprinted here, and the classifier drops the redacted text unless
// rca.deadlock_statement_text is "redacted".

// DeadlockParticipant is rca's participant type, re-exported for callers
// of ParseDeadlockDetail.
type DeadlockParticipant = rca.DeadlockParticipant

// Statement bounds: a statement is clipped to maxStatementBytes before it
// is redacted and fingerprinted; the redacted text kept is at most
// maxStatementRunes runes.
const (
	maxStatementBytes = 2000
	maxStatementRunes = 300
	fingerprintHexLen = 16
)

var (
	reDeadlockWait = regexp.MustCompile(
		`^Process (\d+) waits for (\S+) on (.+); blocked by process (\d+)\.$`)
	reDeadlockStatement = regexp.MustCompile(`^Process (\d+): (.*)$`)
	reContextRelation   = regexp.MustCompile(`\bin relation "((?:[^"]|"")+)"`)
)

// ParseDeadlockDetail reads the participants of a deadlock DETAIL, at most
// rca.MaxDeadlockParticipants, in the order PostgreSQL names them, and
// counts the lines it could not read. A line that continues a statement
// (a multi-line query) belongs to it. A malformed line never drops the
// participants read around it; a non-English DETAIL gives none.
func ParseDeadlockDetail(detail, context string) ([]DeadlockParticipant, int) {
	p := detailParser{index: map[int]int{}}
	for _, line := range strings.Split(detail, "\n") {
		p.line(line)
	}
	p.finishStatement()
	if len(p.parts) > 0 {
		if m := reContextRelation.FindStringSubmatch(context); m != nil {
			p.parts[0].Relation = strings.ReplaceAll(m[1], `""`, `"`)
		}
	}
	return p.parts, p.unparsed
}

// detailParser accumulates participants line by line. stmtPID is the
// process whose statement is being read (0 = none).
type detailParser struct {
	parts    []DeadlockParticipant
	index    map[int]int
	unparsed int
	stmtPID  int
	stmt     strings.Builder
}

func (p *detailParser) line(raw string) {
	line := strings.TrimSpace(raw)
	if m := reDeadlockWait.FindStringSubmatch(line); m != nil {
		p.finishStatement()
		pid, _ := strconv.Atoi(m[1])
		by, _ := strconv.Atoi(m[4])
		if part := p.participant(pid); part != nil {
			part.WaitMode, part.WaitObject, part.BlockedBy = m[2], m[3], by
		}
		return
	}
	if m := reDeadlockStatement.FindStringSubmatch(line); m != nil {
		p.finishStatement()
		p.stmtPID, _ = strconv.Atoi(m[1])
		p.stmt.WriteString(m[2])
		return
	}
	switch {
	case p.stmtPID != 0:
		p.stmt.WriteString("\n" + raw)
	case line != "":
		p.unparsed++
	}
}

// participant returns pid's entry, adding it while there is room, or nil
// past the cap.
func (p *detailParser) participant(pid int) *DeadlockParticipant {
	if i, ok := p.index[pid]; ok {
		return &p.parts[i]
	}
	if len(p.parts) >= rca.MaxDeadlockParticipants {
		return nil
	}
	p.index[pid] = len(p.parts)
	p.parts = append(p.parts, DeadlockParticipant{PID: pid})
	return &p.parts[len(p.parts)-1]
}

// finishStatement fingerprints the statement being read, if any.
func (p *detailParser) finishStatement() {
	if p.stmtPID == 0 {
		return
	}
	pid, stmt := p.stmtPID, p.stmt.String()
	p.stmtPID = 0
	p.stmt.Reset()
	if part := p.participant(pid); part != nil {
		part.Fingerprint, part.Statement = statementFingerprint(stmt)
	}
}

// statementFingerprint clips stmt to maxStatementBytes, strips its
// comments (free text that may name a person and per-request tags that
// would split one shape into many ids), redacts its literals and values,
// collapses whitespace, and returns the first 16 hex characters of the
// sha256 of that text and the text clipped to maxStatementRunes. A blank
// statement gives neither.
func statementFingerprint(stmt string) (fingerprint, text string) {
	clean := llm.RedactSQLLiterals(llm.StripSQLComments(Truncate(stmt, maxStatementBytes)))
	norm := strings.Join(strings.Fields(redactStatementValues(clean)), " ")
	if norm == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:fingerprintHexLen], clipRunes(norm, maxStatementRunes)
}

// clipRunes returns the first n runes of s.
func clipRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// redactStatementValues replaces every numeric constant of a statement
// whose string literals are already redacted with "?": a statement's
// shape, not its values, is what identifies it, and a number may be a card
// or account number. Quoted identifiers, identifiers with digits (t1) and
// $n parameters are kept. A number runs to the end of its token, so a hex
// or exponent form is replaced whole.
func redactStatementValues(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			end := strings.IndexByte(s[i+1:], '"')
			if end < 0 {
				b.WriteString(s[i:])
				return b.String()
			}
			b.WriteString(s[i : i+end+2])
			i += end + 2
		case isDigit(c), c == '.' && i+1 < len(s) && isDigit(s[i+1]):
			b.WriteByte('?')
			i = numberEnd(s, i)
		case isWordByte(c) || c == '$':
			j := i + 1
			for j < len(s) && isWordByte(s[j]) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// isWordByte reports a byte that continues an identifier: a letter, a
// digit, an underscore or a byte of a multi-byte character. A token that
// starts with a digit is a number (redactStatementValues checks first).
func isWordByte(c byte) bool {
	return c == '_' || c >= 0x80 || (c|0x20 >= 'a' && c|0x20 <= 'z') || isDigit(c)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// numberEnd returns the index after the numeric token starting at i:
// digits, letters, underscores and dots, and a sign after an exponent.
func numberEnd(s string, i int) int {
	j := i
	for j < len(s) {
		c := s[j]
		switch {
		case isWordByte(c) || c == '.':
			j++
		case (c == '+' || c == '-') && (s[j-1]|0x20 == 'e') && j+1 < len(s) &&
			isDigit(s[j+1]):
			j++
		default:
			return j
		}
	}
	return j
}
