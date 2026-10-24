package impact

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// run is one Analyze (or InspectStatement) call's state.
type run struct {
	e         *Engine
	ctx       context.Context
	cfg       config.ChangeReviewConfig
	in        changereview.Input
	reader    catalogread.Reader
	version   int
	mode      string
	stmts     []*stmt
	rels      map[string]*relation
	created   map[string]bool
	work      *workload
	ev        []changereview.Evidence
	evIdx     map[string]int
	finds     []changereview.Finding
	nd        []changereview.NotDetermined
	unknown   map[string]string
	predicted map[int64]float64
	err       error
}

// stmt is one statement of the input: its parse-tree parts (one per
// ALTER TABLE subcommand or DROP object) or, in text mode, its regex
// classifications.
type stmt struct {
	index       int
	file        string
	line        int
	sql         string
	kind        string
	parts       []sqlast.DDLStatement
	targets     []string
	text        bool
	classes     []migration.DDLClassification
	fingerprint string
	relations   []string
	planSummary string
	planErr     string
	evID        string
	// noTxn: the statement's file opts out of its migration tool's
	// transaction (txn.go).
	noTxn bool
}

func (e *Engine) newRun(ctx context.Context, cfg config.ChangeReviewConfig,
	in changereview.Input) *run {
	if cfg == (config.ChangeReviewConfig{}) {
		cfg = e.d.Config
	}
	return &run{e: e, ctx: ctx, cfg: cfg, in: in, mode: changereview.ParseAST,
		reader: catalogread.New(e.d.Pool, e.d.Timeouts), rels: map[string]*relation{},
		created: map[string]bool{}, evIdx: map[string]int{}, unknown: map[string]string{}}
}

// addEvidence stores evidence kind:ref once and returns its id ("" when
// the data cannot be encoded, which is logged).
func (r *run) addEvidence(kind, ref, label string, data any) string {
	ev, err := changereview.NewEvidence(kind, evidenceRef(ref), label, data)
	if err != nil {
		r.e.logf("change review impact: evidence %s:%s: %v", kind, ref, err)
		return ""
	}
	if _, ok := r.evIdx[ev.ID]; !ok {
		r.evIdx[ev.ID] = len(r.ev)
		r.ev = append(r.ev, ev)
	}
	return ev.ID
}

// find adds a rule finding citing the non-empty ids.
func (r *run) find(rule, severity, title, text string, ids ...string) {
	var cited []string
	for _, id := range ids {
		if id != "" && !hasString(cited, id) {
			cited = append(cited, id)
		}
	}
	r.finds = append(r.finds, newFinding(rule, severity, title, text, cited...))
}

func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// readFailed sorts a read error: a context that ended stops the run
// (true); anything else (a timeout, a missing privilege, a missing
// extension) is recorded as unknown for subject and the run goes on.
func (r *run) readFailed(err error, s *stmt, subject string) bool {
	if err == nil {
		return false
	}
	if r.ctx.Err() != nil {
		if r.err == nil {
			r.err = r.ctx.Err()
		}
		return true
	}
	reason, fix := classifyReadError(err)
	r.unknownFact(s, subject, reason, err.Error(), fix)
	return true
}

// classifyReadError is a failed read's reason and what fixes it.
func classifyReadError(err error) (string, string) {
	var pe *catalogread.PhaseError
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pe):
		return "timed out", "the read hit safety.query_timeout_ms or lock_timeout_ms; " +
			"review again when the table is not locked"
	case errors.As(err, &pgErr) && pgErr.Code == "42501":
		return "missing privileges", "grant pg_sage's role SELECT on the table " +
			"and pg_read_all_stats"
	case errors.Is(err, pgx.ErrNoRows):
		return "not found", ""
	}
	return "read failed", ""
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// unknownFact records that subject could not be determined: a CR-17
// warning citing the statement, an "unknown:" evidence and extra (the
// statements concerned), once per subject, reason and statement.
func (r *run) unknownFact(s *stmt, subject, reason, detail, fix string,
	extra ...string) string {
	key := subject + "\x00" + reason
	if s != nil {
		key = fmt.Sprintf("%d\x00%s", s.index, key)
	}
	if id, ok := r.unknown[key]; ok {
		return id
	}
	slug := strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(reason+"-"+subject),
		"_"), "_")
	stmtID := ""
	if s != nil {
		slug = fmt.Sprintf("%s.%d", slug, s.index)
		stmtID = s.evID
	}
	id := r.addEvidence("unknown", slug, subject+": "+reason,
		unknownData{Subject: subject, Reason: reason, Detail: detail, Fix: fix})
	r.unknown[key] = id
	r.find(RuleImpactUnknown, changereview.SeverityWarning,
		"Impact unknown: "+subject+" ("+reason+")", unknownText(subject, reason, detail, fix),
		append([]string{stmtID, id}, extra...)...)
	r.nd = append(r.nd, changereview.NotDetermined{Subject: subject, Reason: reason})
	return id
}

func unknownText(subject, reason, detail, fix string) string {
	t := subject + ": " + reason
	if detail != "" {
		t += " (" + detail + ")"
	}
	t += ". Unknown is not none: this change is not proven safe."
	if fix != "" {
		t += " To know: " + fix + "."
	}
	return t
}

// analysis is the run's result.
func (r *run) analysis() changereview.Analysis {
	exp := r.expectations()
	exp.AtReview = r.atReview(exp)
	a := changereview.Analysis{ParseMode: r.mode, Evidence: r.ev, Findings: r.finds,
		NotDetermined: r.nd, Statements: []changereview.Statement{}, Expectations: exp}
	for _, s := range r.stmts {
		a.Statements = append(a.Statements, r.statement(s))
	}
	return a
}

// statement is s as the review stores it.
func (r *run) statement(s *stmt) changereview.Statement {
	out := changereview.Statement{Index: s.index, Origin: changereview.OriginSQLInput,
		File: s.file, Line: s.line, SQL: s.sql, Kind: s.kind,
		Relations: append([]string{}, s.relations...), Fingerprint: s.fingerprint,
		PlanSummary: s.planSummary, EvidenceID: s.evID, Confidence: 1}
	if r.work != nil && s.fingerprint != "" {
		if id, ok := r.work.byFingerprint[s.fingerprint]; ok {
			out.MatchedQueryID = &id
		}
	}
	return out
}
