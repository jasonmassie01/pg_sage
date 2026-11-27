package briefing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/pg-sage/sidecar/internal/llm"
)

// llm-08: the LLM rewrite of a briefing may repeat pg_sage's own vetted
// recommendations, never invent SQL. Code spans and fenced blocks whose
// statements are not among the payload's recommendations are replaced;
// prose is never touched.

const (
	removedSQLMarker     = "[SQL removed: not a pg_sage recommendation, see %s]"
	removedCommandMarker = "[command removed: not a pg_sage recommendation, see %s]"
	noCitation           = "the Cases page"
)

// statementKeywords are the first tokens that make a code span a statement.
var statementKeywords = toSet("ABORT", "ALTER", "ANALYZE", "BEGIN", "CALL",
	"CHECKPOINT", "CLUSTER", "COMMENT", "COMMIT", "COPY", "CREATE", "DEALLOCATE",
	"DELETE", "DISCARD", "DO", "DROP", "END", "EXECUTE", "EXPLAIN", "GRANT",
	"IMPORT", "INSERT", "LOAD", "LOCK", "MERGE", "NOTIFY", "PREPARE", "REASSIGN",
	"REFRESH", "REINDEX", "RELEASE", "RESET", "REVOKE", "ROLLBACK", "SAVEPOINT",
	"SECURITY", "SELECT", "SET", "SHOW", "START", "TABLE", "TRUNCATE", "UPDATE",
	"VACUUM", "VALUES", "WITH")

// standaloneCommands are statements on their own; any other keyword needs
// at least one more token (`SELECT` alone is a word, not a statement).
var standaloneCommands = toSet("ANALYZE", "CHECKPOINT", "CLUSTER", "VACUUM")

// changingCommands change data, schema, settings or server state. Code
// naming one anywhere is checked against the recommendations.
var changingCommands = toSet("ALTER", "CHECKPOINT", "CLUSTER", "COPY", "CREATE",
	"DELETE", "DROP", "GRANT", "INSERT", "LOCK", "MERGE", "REFRESH", "REINDEX",
	"RESET", "REVOKE", "SET", "TRUNCATE", "UPDATE", "VACUUM")

// toolCommands are client utilities, shell commands and admin functions
// that change the server without naming a SQL command: `vacuumdb --full`
// is VACUUM FULL by another name. Code running one is checked against the
// recommendations; a tool named on its own is a name.
var toolCommands = toSet("CLUSTERDB", "CREATEDB", "CREATEUSER", "DROPDB", "DROPUSER",
	"KILL", "KILLALL", "PG_BASEBACKUP", "PG_CANCEL_BACKEND", "PG_CTL", "PG_CTLCLUSTER",
	"PG_DROP_REPLICATION_SLOT", "PG_DROPCLUSTER", "PG_PROMOTE", "PG_RELOAD_CONF",
	"PG_REPACK", "PG_RESETWAL", "PG_RESTORE", "PG_REWIND", "PG_STAT_RESET",
	"PG_STAT_STATEMENTS_RESET", "PG_SWITCH_WAL", "PG_TERMINATE_BACKEND", "PG_UPGRADE",
	"PKILL", "REINDEXDB", "RM", "SET_CONFIG", "SUDO", "SYSTEMCTL", "VACUUMDB")

// commandLanguages tag fenced blocks of shell commands or configuration
// files. pg_sage recommends neither (its recommendations are SQL), so such a
// block is never grounded.
var commandLanguages = toSet("BASH", "BAT", "CMD", "CONSOLE", "POWERSHELL", "PS1",
	"PWSH", "SH", "SHELL", "ZSH", "CFG", "CONF", "INI", "PROPERTIES", "TOML", "YAML",
	"YML")

// settingRE finds a setting assignment such as "work_mem = 256MB" or
// "-c fsync=off": a name, then "=" that is not part of "==", "=>", ">=",
// "<=" or "!=".
var settingRE = regexp.MustCompile(`(^|[^\w.=<>!])[A-Za-z_]\w*(\.\w+)?[ \t]*=[ \t]*[^=>\s]`)

// citationRE finds "#<id>" not inside a link label or an HTML entity.
var citationRE = regexp.MustCompile(`(^|[^\w\[&#/])#([0-9]+)\b`)

var blankLineRE = regexp.MustCompile(`\n[ \t\r]*\n`)

func toSet(words ...string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

// briefWithLLM asks the LLM for the briefing and grounds the answer. It
// returns the text to store (citations linked to the Cases page) and the
// plain text to dispatch (Slack and stdout have no dashboard URL).
func (w *Worker) briefWithLLM(ctx context.Context, p Payload) (
	stored, plain string, tokens int, err error,
) {
	data, err := p.promptJSON()
	if err != nil {
		return "", "", 0, err
	}
	text, tokens, err := w.enhanceWithLLM(ctx, data)
	if err != nil {
		return "", "", tokens, err
	}
	plain, removed := groundOutput(text, p)
	if len(removed) > 0 {
		w.logFn("WARN", "briefing: removed %d statement(s) the LLM proposed that are "+
			"not pg_sage recommendations (digest %s)", len(removed), digest(removed))
	}
	return linkCitations(plain, p), plain, tokens, nil
}

// digest identifies removed statements in logs without logging them.
func digest(statements []string) string {
	sum := sha256.Sum256([]byte(strings.Join(statements, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// groundOutput replaces every statement in a code span or fenced block of
// text that matches none of p's recommendations, and returns the removed
// statements.
func groundOutput(text string, p Payload) (string, []string) {
	allowed, ids := p.allowedStatements(), p.byID()
	var b strings.Builder
	var removed []string
	last := 0
	for _, r := range codeRegions(text) {
		repl, gone := groundRegion(text, r, allowed, ids)
		if len(gone) == 0 {
			continue
		}
		b.WriteString(text[last:r.start])
		b.WriteString(repl)
		last = r.end
		removed = append(removed, gone...)
	}
	if len(removed) == 0 {
		return text, nil
	}
	b.WriteString(text[last:])
	return b.String(), removed
}

func groundRegion(text string, r codeRegion, allowed map[string]bool,
	ids map[int64]FindingRef,
) (string, []string) {
	body := text[r.bodyStart:r.bodyEnd]
	cite := func() string { return citationFor(text, r, ids) }
	open, closing := text[r.start:r.bodyStart], text[r.bodyEnd:r.end]
	switch {
	case r.fence && commandLanguages[strings.ToUpper(r.lang)] &&
		strings.TrimSpace(body) != "":
		return open + fmt.Sprintf(removedCommandMarker, cite()) + "\n" + closing,
			[]string{body}
	case r.fence:
		newBody, gone := rewriteStatements(body, allowed, cite)
		return open + newBody + closing, gone
	}
	if _, gone := rewriteStatements(body, allowed, cite); len(gone) > 0 {
		return fmt.Sprintf(removedSQLMarker, cite()), gone
	}
	return "", nil
}

// rewriteStatements replaces each statement of body that is not allowed
// with the removal marker, keeping the whitespace around it.
func rewriteStatements(body string, allowed map[string]bool, cite func() string) (
	string, []string,
) {
	var b strings.Builder
	var gone []string
	for _, piece := range llm.SplitStatements(body) {
		core := strings.TrimSpace(piece)
		vetted := allowed[normalizeStatement(core)] && !commentProposesChange(core)
		if !needsGrounding(core) || vetted {
			b.WriteString(piece)
			continue
		}
		lead := len(piece) - len(strings.TrimLeftFunc(piece, unicode.IsSpace))
		b.WriteString(piece[:lead])
		fmt.Fprintf(&b, removedSQLMarker, cite())
		b.WriteString(piece[lead+len(core):])
		gone = append(gone, core)
	}
	return b.String(), gone
}

// isStatement reports SQL text whose first token is a statement keyword.
func isStatement(sql string) bool {
	s := strings.TrimSpace(llm.StripSQLComments(sql))
	end := 0
	for end < len(s) && isWordByte(s[end]) {
		end++
	}
	word := strings.ToUpper(s[:end])
	if !statementKeywords[word] {
		return false
	}
	rest := strings.TrimSpace(strings.TrimRight(s[end:], "; \t\r\n"))
	return rest != "" || standaloneCommands[word]
}

// needsGrounding reports code that proposes a statement, a change, a
// setting or a tool command, so it must match a recommendation to stay.
func needsGrounding(code string) bool {
	return isStatement(code) || namesWord(code, changingCommands) ||
		settingRE.MatchString(code) || runsTool(code)
}

// runsTool reports code that runs a tool command: it names one and is more
// than a single identifier ("pg_terminate_backend(42)", not
// "pg_terminate_backend").
func runsTool(code string) bool {
	bare := true
	for i := 0; i < len(code); i++ {
		bare = bare && isWordByte(code[i])
	}
	return !bare && namesWord(code, toolCommands)
}

// commentProposesChange reports a comment that names a changing command
// or a tool. Matching ignores comments, so such a comment added to a vetted
// statement would otherwise ride along with it unvetted.
func commentProposesChange(code string) bool {
	guarded := func(s string) int {
		return countWords(s, changingCommands) + countWords(s, toolCommands)
	}
	return guarded(code) > guarded(llm.StripSQLComments(code))
}

// namesWord reports code that names one of words anywhere, as a whole
// word and even inside quotes: "Step 1: DROP TABLE x" or
// psql --command="VACUUM FULL t" do not start with a keyword but still
// propose a change. Code only; prose is never checked.
func namesWord(code string, words map[string]bool) bool {
	return countWords(code, words) > 0
}

// countWords counts the whole words of code that are in words.
func countWords(code string, words map[string]bool) int {
	n := 0
	for i := 0; i < len(code); {
		if !isWordByte(code[i]) {
			i++
			continue
		}
		end := i
		for end < len(code) && isWordByte(code[end]) {
			end++
		}
		if words[strings.ToUpper(code[i:end])] {
			n++
		}
		i = end
	}
	return n
}

// isWordByte is an identifier byte; a non-ASCII byte continues the word,
// so "SELECTé" is not the keyword SELECT.
func isWordByte(c byte) bool {
	return c == '_' || c >= 0x80 || (c >= '0' && c <= '9') ||
		(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// normalizeStatement folds comments, case, whitespace and trailing
// semicolons, so a recommendation re-typed by the LLM still matches.
func normalizeStatement(sql string) string {
	s := strings.ToUpper(strings.Join(strings.Fields(llm.StripSQLComments(sql)), " "))
	s = strings.TrimRight(s, "; ")
	for _, punct := range []string{"(", ")", ",", "="} {
		s = strings.ReplaceAll(s, " "+punct, punct)
		s = strings.ReplaceAll(s, punct+" ", punct)
	}
	return s
}

// citationFor names the known finding cited nearest to r in its paragraph
// (for a fenced block, also the paragraph before it), or the Cases page.
func citationFor(text string, r codeRegion, ids map[int64]FindingRef) string {
	start, blank := paragraphStart(text, r.start)
	end := len(text)
	if loc := blankLineRE.FindStringIndex(text[r.end:]); loc != nil {
		end = r.end + loc[0]
	}
	if id, ok := lastCitation(text[start:r.start], ids); ok {
		return "finding #" + strconv.FormatInt(id, 10)
	}
	if id, ok := firstCitation(text[r.end:end], ids); ok {
		return "finding #" + strconv.FormatInt(id, 10)
	}
	if r.fence && blank > 0 {
		prev, _ := paragraphStart(text, blank)
		if id, ok := lastCitation(text[prev:blank], ids); ok {
			return "finding #" + strconv.FormatInt(id, 10)
		}
	}
	return noCitation
}

// paragraphStart returns where the paragraph holding offset begins and
// where the blank line before it begins (0 when there is none).
func paragraphStart(text string, offset int) (int, int) {
	locs := blankLineRE.FindAllStringIndex(text[:offset], -1)
	if len(locs) == 0 {
		return 0, 0
	}
	last := locs[len(locs)-1]
	return last[1], last[0]
}

func citations(text string, ids map[int64]FindingRef) []int64 {
	var out []int64
	for _, m := range citationRE.FindAllStringSubmatch(text, -1) {
		id, err := strconv.ParseInt(m[2], 10, 64)
		if _, known := ids[id]; err == nil && known {
			out = append(out, id)
		}
	}
	return out
}

func lastCitation(text string, ids map[int64]FindingRef) (int64, bool) {
	found := citations(text, ids)
	if len(found) == 0 {
		return 0, false
	}
	return found[len(found)-1], true
}

func firstCitation(text string, ids map[int64]FindingRef) (int64, bool) {
	found := citations(text, ids)
	if len(found) == 0 {
		return 0, false
	}
	return found[0], true
}

// linkCitations links each prose "#<id>" of a known finding to its case on
// the dashboard Cases page. Code is left as written.
func linkCitations(text string, p Payload) string {
	ids := p.byID()
	regions := codeRegions(text)
	var b strings.Builder
	last := 0
	for _, m := range citationRE.FindAllStringSubmatchIndex(text, -1) {
		hash, numEnd := m[4]-1, m[5]
		id, err := strconv.ParseInt(text[m[4]:numEnd], 10, 64)
		f, known := ids[id]
		if err != nil || !known || inRegion(regions, hash) {
			continue
		}
		b.WriteString(text[last:hash])
		fmt.Fprintf(&b, "[#%d](%s)", id, caseLink(f.CaseKey, p.database))
		last = numEnd
	}
	b.WriteString(text[last:])
	return b.String()
}

// caseLink is the dashboard route to a case. Like the dashboard's own case
// links it names the database, so the link opens the case even when the
// viewer has another database selected.
func caseLink(key, database string) string {
	link := "#/cases?case=" + url.QueryEscape(key)
	if database != "" {
		link += "&database=" + url.QueryEscape(database)
	}
	return link
}

func inRegion(regions []codeRegion, offset int) bool {
	for _, r := range regions {
		if offset >= r.start && offset < r.end {
			return true
		}
	}
	return false
}
