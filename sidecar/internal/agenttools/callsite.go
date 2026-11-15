package agenttools

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/codehost"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// CallSiteConfidence says how sure a call site is.
type CallSiteConfidence string

// Call-site confidences. Only one exact match counts as found.
const (
	CallSiteExact  CallSiteConfidence = "exact"
	CallSiteLikely CallSiteConfidence = "likely"
	CallSiteNone   CallSiteConfidence = "none"
)

// Call-site methods, in the order FindCallSite trusts them.
const (
	CallSiteMethodFileLine = "sqlcommenter file and line"
	CallSiteMethodLiteral  = "SQL literal with the same fingerprint"
	CallSiteMethodRoute    = "sqlcommenter controller, action or route search"
	CallSiteMethodNone     = "none"
)

// Bounds of one lookup.
const (
	// maxCallSiteFiles is how many search hits one rule reads.
	maxCallSiteFiles = 5
	// maxTagLine is the largest line number a tag may name.
	maxTagLine = 10_000_000
)

const callSiteGuidance = "No call site: add sqlcommenter tags to the application's " +
	"queries (file and line, or controller, action and route), or keep the statement " +
	"as one SQL literal in the code so it can be matched."

// CallSite is where in the application's repository a statement is sent.
// Path and Line are set for an exact match and for a single likely one;
// several likely matches are listed in Candidates and never guessed.
type CallSite struct {
	QueryID    QueryID             `json:"query_id"`
	Path       string              `json:"path,omitempty"`
	Line       int                 `json:"line,omitempty"`
	Method     string              `json:"method"`
	Confidence CallSiteConfidence  `json:"confidence"`
	Candidates []CallSiteCandidate `json:"candidates,omitempty"`
	Note       string              `json:"note,omitempty"`
}

// CallSiteCandidate is one place a statement may be sent from.
type CallSiteCandidate struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

// Summary is the one line the query page and the case show.
func (c CallSite) Summary() string {
	switch {
	case c.Path != "":
		return fmt.Sprintf("Called from %s:%d (%s)", c.Path, c.Line, c.Confidence)
	case len(c.Candidates) > 0:
		places := make([]string, 0, len(c.Candidates))
		for _, cand := range c.Candidates {
			places = append(places, fmt.Sprintf("%s:%d", cand.Path, cand.Line))
		}
		return fmt.Sprintf("Called from one of %d places (%s): %s", len(c.Candidates),
			c.Confidence, strings.Join(places, ", "))
	}
	return "No call site found (none): " + c.Note
}

// callSiteTags is what the finder reads from the sqlcommenter tags.
type callSiteTags struct {
	file       string
	line       int
	controller string
	action     string
	route      string
}

// FindCallSite traces one statement to the application code that sends
// it. It tries, in order: a file plus line tag (Rails marginalia style),
// read with GetFile and checked against the file's length; a SQL literal
// in the code whose fingerprint matches normalizedSQL (the
// pg_stat_statements text); and a search for the controller, action or
// route tags, which is never more than likely. Invalid tags (a line that
// is not a positive number, an absolute path or one with "..") are
// refused before any request. A code-host error other than "not found" is
// returned; a statement with nothing to search for is CallSiteNone.
func FindCallSite(ctx context.Context, host codehost.Host, repo string, queryID QueryID,
	tags map[string]string, normalizedSQL string) (CallSite, error) {
	if host == nil {
		return CallSite{}, fmt.Errorf("%w: no code host is configured", ErrUnavailable)
	}
	if err := codehost.ValidateRepo(repo); err != nil {
		return CallSite{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	ct, err := readCallSiteTags(tags)
	if err != nil {
		return CallSite{}, err
	}
	f := finder{host: host, repo: repo, id: queryID}
	res, err := f.find(ctx, ct, normalizedSQL)
	if err != nil {
		return CallSite{}, fmt.Errorf("find call site of query %d in %s via %s: %w",
			queryID, repo, host.Name(), err)
	}
	return res, nil
}

// readCallSiteTags checks the tags the finder uses.
func readCallSiteTags(tags map[string]string) (callSiteTags, error) {
	ct := callSiteTags{file: strings.TrimPrefix(strings.TrimSpace(tags["file"]), "./"),
		controller: searchable(tags["controller"]), action: searchable(tags["action"]),
		route: searchable(tags["route"])}
	if ct.file == "" {
		return ct, nil
	}
	if err := codehost.ValidatePath(ct.file); err != nil {
		return ct, fmt.Errorf("%w: sqlcommenter file tag: %v", ErrInvalid, err)
	}
	raw, ok := tags["line"]
	if !ok {
		return ct, nil
	}
	line, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || line < 1 || line > maxTagLine {
		return ct, fmt.Errorf("%w: sqlcommenter line tag %q is not a line number from 1 "+
			"to %d", ErrInvalid, raw, maxTagLine)
	}
	ct.line = line
	return ct, nil
}

// searchable keeps a tag value usable as a search term.
func searchable(v string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '"' || r < 0x20 {
			return ' '
		}
		return r
	}, v))
}

// finder runs the rules of one lookup.
type finder struct {
	host codehost.Host
	repo string
	id   QueryID
}

func (f finder) find(ctx context.Context, ct callSiteTags, sql string) (CallSite, error) {
	var notes []string
	if ct.file != "" && ct.line > 0 {
		res, note, err := f.byFileLine(ctx, ct)
		if err != nil || res.Confidence == CallSiteExact {
			return res, err
		}
		notes = append(notes, note)
	}
	literal, err := f.byLiteral(ctx, sql)
	if err != nil || literal.Confidence == CallSiteExact {
		return literal, err
	}
	route, err := f.byRoute(ctx, ct)
	if err != nil || route.Confidence == CallSiteLikely {
		return route, err
	}
	if literal.Confidence == CallSiteLikely {
		return literal, nil
	}
	notes = append(notes, callSiteGuidance)
	return CallSite{QueryID: f.id, Method: CallSiteMethodNone, Confidence: CallSiteNone,
		Note: strings.Join(notes, " ")}, nil
}

// byFileLine reads the tagged file and checks the line exists.
func (f finder) byFileLine(ctx context.Context, ct callSiteTags,
) (CallSite, string, error) {
	file, err := f.host.GetFile(ctx, f.repo, ct.file)
	if errors.Is(err, codehost.ErrNotFound) {
		return CallSite{}, fmt.Sprintf("The tagged file %s is not in %s.", ct.file, f.repo),
			nil
	}
	if err != nil {
		return CallSite{}, "", err
	}
	n := lineCount(file.Content)
	if ct.line > n {
		return CallSite{}, fmt.Sprintf("The tagged line %d is past the end of %s (%d "+
			"lines).", ct.line, ct.file, n), nil
	}
	return CallSite{QueryID: f.id, Path: ct.file, Line: ct.line,
		Method: CallSiteMethodFileLine, Confidence: CallSiteExact}, "", nil
}

// lineCount counts lines; a last line without a newline counts.
func lineCount(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// byRoute searches the controller and action tags (else the route) and
// lists the first line of each hit holding the most specific term.
func (f finder) byRoute(ctx context.Context, ct callSiteTags) (CallSite, error) {
	terms := nonEmpty(ct.controller, ct.action)
	query := strings.Join(terms, " ")
	if len(terms) == 0 && ct.route != "" {
		terms, query = []string{ct.route}, `"`+ct.route+`"`
	}
	if len(terms) == 0 {
		return CallSite{}, nil
	}
	needle := strings.ToLower(terms[len(terms)-1])
	var cands []CallSiteCandidate
	err := f.eachHit(ctx, query, func(file codehost.File) {
		for i, line := range strings.Split(file.Content, "\n") {
			if strings.Contains(strings.ToLower(line), needle) {
				cands = append(cands, CallSiteCandidate{Path: file.Path, Line: i + 1})
				return
			}
		}
	})
	if err != nil || len(cands) == 0 {
		return CallSite{}, err
	}
	return f.candidates(CallSiteMethodRoute, CallSiteLikely, cands), nil
}

// byLiteral searches the statement's identifiers and matches the string
// literals of each hit by fingerprint; one match is exact.
func (f finder) byLiteral(ctx context.Context, sql string) (CallSite, error) {
	want := sqlFingerprint(sql)
	terms := searchTerms(sql)
	if want == "" || len(terms) == 0 {
		return CallSite{}, nil
	}
	var cands []CallSiteCandidate
	err := f.eachHit(ctx, strings.Join(terms, " "), func(file codehost.File) {
		for _, lit := range codeLiterals(file.Content) {
			if sqlFingerprint(lit.text) == want {
				cands = append(cands, CallSiteCandidate{Path: file.Path, Line: lit.line})
			}
		}
	})
	if err != nil || len(cands) == 0 {
		return CallSite{}, err
	}
	conf := CallSiteExact
	if len(cands) > 1 {
		conf = CallSiteLikely
	}
	return f.candidates(CallSiteMethodLiteral, conf, cands), nil
}

// eachHit searches query and reads up to maxCallSiteFiles hits; a hit
// that vanished between search and read is skipped.
func (f finder) eachHit(ctx context.Context, query string, visit func(codehost.File)) error {
	hits, err := f.host.SearchCode(ctx, f.repo, sqlast.ClipBytes(query, 256))
	if err != nil {
		return err
	}
	for i, hit := range hits {
		if i == maxCallSiteFiles {
			break
		}
		file, err := f.host.GetFile(ctx, f.repo, hit.Path)
		if errors.Is(err, codehost.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		visit(file)
	}
	return nil
}

// candidates builds the result: one candidate is placed, several only listed.
func (f finder) candidates(method string, conf CallSiteConfidence,
	cands []CallSiteCandidate) CallSite {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Path != cands[j].Path {
			return cands[i].Path < cands[j].Path
		}
		return cands[i].Line < cands[j].Line
	})
	res := CallSite{QueryID: f.id, Method: method, Confidence: conf, Candidates: cands}
	if len(cands) == 1 {
		res.Path, res.Line = cands[0].Path, cands[0].Line
		if conf == CallSiteExact {
			res.Candidates = nil
		}
	}
	return res
}

func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
