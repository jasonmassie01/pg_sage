package changereview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// InputHash identifies one request at one commit: the database, the SQL
// (inline and files with their paths), the diff's sha256, the PR URL and
// head commit, the migration tool and the transactional override. Title,
// PR number and wait are not part of it. A repeat within DedupeWindow
// reuses the completed review.
func InputHash(database string, r Request) string {
	type file struct {
		Path    string `json:"p"`
		Content string `json:"c"`
	}
	v := struct {
		V             int    `json:"v"`
		Database      string `json:"db"`
		SQL           string `json:"sql"`
		Files         []file `json:"files"`
		Diff          string `json:"diff"`
		PRURL         string `json:"pr"`
		HeadSHA       string `json:"head"`
		Tool          string `json:"tool"`
		Transactional *bool  `json:"tx"`
	}{V: 1, Database: database, SQL: r.SQL, Diff: DiffSHA256(r.Diff), PRURL: r.PR.URL,
		HeadSHA: r.PR.HeadSHA, Tool: strings.ToLower(strings.TrimSpace(r.MigrationTool)),
		Transactional: r.Transactional, Files: []file{}}
	for _, f := range r.SQLFiles {
		v.Files = append(v.Files, file{Path: f.Path, Content: f.Content})
	}
	raw, _ := json.Marshal(v) // plain strings and bools always marshal
	return sha256Hex(raw)
}

// ContentHash identifies the database-relevant content of a change across
// revisions: one key per SQL statement in order (a parse-tree key that
// ignores comments, whitespace and keyword case but keeps literals and
// type modifiers, or a normalized-text key when the statement does not
// parse or the build has no parser), file paths excluded, plus the
// sha256 of the redacted data-access hunks. A DBA verdict carries over to
// a revision only when this is equal.
func ContentHash(files []SQLFile, dbHunks string) string {
	var b strings.Builder
	b.WriteString("v1\n")
	for _, f := range files {
		keys, _ := StatementKeys(f.Content)
		for _, k := range keys {
			b.WriteString(k)
			b.WriteByte('\n')
		}
	}
	b.WriteString("hunks:")
	b.WriteString(DiffSHA256(dbHunks))
	return sha256Hex([]byte(b.String()))
}

// StatementKeys is one key per statement of sql, and whether they come
// from the parse tree. A script the parser rejects is keyed by text.
func StatementKeys(sql string) ([]string, bool) {
	if keys, ok := astKeys(sql); ok {
		return keys, true
	}
	var keys []string
	for _, stmt := range textStatements(sql) {
		keys = append(keys, "t:"+sha256Hex([]byte(stmt)))
	}
	return keys, false
}

// DiffSHA256 is the hex sha256 of diff, or "" for an empty diff.
func DiffSHA256(diff string) string {
	if diff == "" {
		return ""
	}
	return sha256Hex([]byte(diff))
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// receiptSeal is what receipt_hash covers: the review's identity, its
// verdicts and every jsonb column, the landing bracket and the receipt.
// The DBA verdict, credit, shadow score and comment are recorded or
// re-rendered later and are not sealed.
type receiptSeal struct {
	ID                   string          `json:"id"`
	Database             string          `json:"database"`
	InputHash            string          `json:"input_hash"`
	ContentHash          string          `json:"content_hash"`
	Verdict              string          `json:"verdict"`
	DeterministicVerdict string          `json:"deterministic_verdict"`
	ParseMode            string          `json:"parse_mode"`
	Statements           json.RawMessage `json:"statements"`
	Findings             json.RawMessage `json:"findings"`
	Evidence             json.RawMessage `json:"evidence"`
	Dropped              json.RawMessage `json:"dropped"`
	NotDetermined        json.RawMessage `json:"not_determined"`
	Summary              json.RawMessage `json:"summary"`
	Expectations         json.RawMessage `json:"expectations"`
	Transcript           json.RawMessage `json:"transcript"`
	LandingState         string          `json:"landing_state"`
	LandingSource        string          `json:"landing_source"`
	LandedLower          string          `json:"landed_lower"`
	LandedAt             string          `json:"landed_at"`
	ReceiptVerdict       string          `json:"receipt_verdict"`
	Receipt              json.RawMessage `json:"receipt"`
	ReceiptDecidedAt     string          `json:"receipt_decided_at"`
}

// ReceiptHash is the hex sha256 of r's sealed fields in canonical JSON.
func ReceiptHash(r Review) (string, error) {
	cols, err := jsonColumns(r)
	if err != nil {
		return "", err
	}
	seal := receiptSeal{ID: r.ID, Database: r.Database, InputHash: r.InputHash,
		ContentHash: r.ContentHash, Verdict: r.Verdict,
		DeterministicVerdict: r.DeterministicVerdict, ParseMode: r.ParseMode,
		Statements: cols.statements, Findings: cols.findings, Evidence: cols.evidence,
		Dropped: cols.dropped, NotDetermined: cols.notDetermined, Summary: cols.summary,
		Expectations: cols.expectations, Transcript: cols.transcript,
		LandingState: r.LandingState, LandingSource: r.LandingSource,
		LandedLower: stamp(r.LandedLower), LandedAt: stamp(r.LandedAt),
		ReceiptVerdict: r.ReceiptVerdict, Receipt: cols.receipt,
		ReceiptDecidedAt: stamp(r.ReceiptDecidedAt)}
	raw, err := json.Marshal(seal)
	if err != nil {
		return "", err
	}
	canon, err := CanonicalJSON(json.RawMessage(raw))
	if err != nil {
		return "", err
	}
	return sha256Hex(canon), nil
}

// VerifyReceipt reports whether r carries a receipt hash that still
// matches its sealed fields.
func VerifyReceipt(r Review) bool {
	if r.ReceiptHash == "" {
		return false
	}
	h, err := ReceiptHash(r)
	return err == nil && h == r.ReceiptHash
}

// stamp renders a timestamp at the store's microsecond precision.
func stamp(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}
