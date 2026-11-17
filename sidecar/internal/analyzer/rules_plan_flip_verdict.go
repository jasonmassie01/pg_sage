package analyzer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The re-ANALYZE's verdict, judged from the first plan captured after it
// (guard-02c). The capture follows within one capture interval: the plan
// capture is nudged after any ANALYZE of a relation the query reads.
const (
	verdictOpen     = "open"      // not re-analyzed yet and still flipped
	verdictCleared  = "cleared"   // the plan moved off the flip without pg_sage
	verdictPending  = "pending"   // re-analyzed; no comparable plan since
	verdictNoChange = "no_change" // re-analyzed; the flipped plan stayed
	verdictImproved = "improved"  // re-analyzed; the plan moved, blocks not worse
)

const (
	// detailReanalyzeVerdict holds the verdict in the finding detail.
	detailReanalyzeVerdict = "reanalyze_verdict"
	// handedToPlanStability names review-02's plan-stability generator,
	// which proposes a per-column statistics target for a flip one
	// ANALYZE did not fix.
	handedToPlanStability = "plan_stability"
)

// openFlip is a flip finding still open, or resolved by pg_sage's
// re-ANALYZE and not judged yet, with that ANALYZE (ActionID 0: none).
type openFlip struct {
	ID                             int64
	Ident, Status, Severity, Title string
	Detail                         map[string]any
	ActionID                       int64
	ActionAt                       time.Time
}

// latestCapture is the query's newest plan fingerprint and its time.
type latestCapture struct {
	Hash string
	At   time.Time
}

// judgeReanalyze decides an open flip from the query's newest capture
// and its blocks per call in the full hours after the re-ANALYZE.
func judgeReanalyze(o openFlip, latest *latestCapture, blocksAfter *float64) string {
	flipped, _ := o.Detail["current_plan_hash"].(string)
	if flipped == "" {
		return verdictOpen
	}
	comparable := latest != nil && latest.Hash != "" &&
		hashVersion(latest.Hash) == hashVersion(flipped)
	handedOn := o.Detail[detailReanalyzeVerdict] == verdictNoChange
	if o.ActionID == 0 || handedOn {
		switch {
		case comparable && latest.Hash != flipped:
			return verdictCleared
		case handedOn:
			return verdictNoChange
		}
		return verdictOpen
	}
	if !comparable || !latest.At.After(o.ActionAt) {
		return verdictPending
	}
	if latest.Hash == flipped {
		return verdictNoChange
	}
	if during, ok := flipDetailNumber(o.Detail["blocks_per_call_after"]); ok && blocksAfter != nil &&
		*blocksAfter > during {
		return verdictNoChange
	}
	return verdictImproved
}

// decodeFlipDetail decodes a stored finding detail keeping numbers exact:
// a pg_stat_statements queryid is a 64-bit hash that float64 would round.
func decodeFlipDetail(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var d map[string]any
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	return d, nil
}

// flipDetailNumber reads a detail number decoded exactly (json.Number) or built
// in memory (float64).
func flipDetailNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// flipQueryID is the open flip's queryid, from its identity "queryid:N"
// (exact, unlike a float64 detail value); false when it names none.
func flipQueryID(o openFlip) (int64, bool) {
	s, ok := strings.CutPrefix(o.Ident, "queryid:")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}

// verdictDetail is the open flip's detail with the verdict recorded; the
// stored map is copied, never written.
func verdictDetail(o openFlip, verdict string, latest *latestCapture,
	blocksAfter *float64) map[string]any {
	d := make(map[string]any, len(o.Detail)+5)
	for k, v := range o.Detail {
		d[k] = v
	}
	d[detailReanalyzeVerdict] = verdict
	if o.ActionID > 0 {
		d["reanalyze_action_id"] = o.ActionID
		d["reanalyzed_at"] = o.ActionAt.UTC().Format(time.RFC3339Nano)
	}
	if latest != nil {
		d["after_plan_hash"] = latest.Hash
		d["after_captured_at"] = latest.At.UTC().Format(time.RFC3339Nano)
	}
	if blocksAfter != nil {
		d["blocks_per_call_after_reanalyze"] = *blocksAfter
	}
	if verdict == verdictNoChange {
		d["handed_to"] = handedToPlanStability
	}
	return d
}

// followUpFinding re-emits a flip one re-ANALYZE did not fix: kept open,
// handed to the plan-stability review, never a second ANALYZE.
func followUpFinding(o openFlip, verdict string, latest *latestCapture,
	blocksAfter *float64) Finding {
	rel, _ := o.Detail["relation"].(string)
	qid, _ := flipQueryID(o)
	return Finding{Category: categoryPlanFlip, Severity: o.Severity, ObjectType: "query",
		ObjectIdentifier: o.Ident, Title: o.Title,
		Detail: verdictDetail(o, verdict, latest, blocksAfter),
		Recommendation: fmt.Sprintf("One re-ANALYZE of %s did not restore the earlier "+
			"plan of query %d, so pg_sage does not run it again. The plan-stability "+
			"review proposes a larger per-column statistics target "+
			"(set_column_statistics) or extended statistics for the columns this "+
			"query filters on.", rel, qid)}
}

// analyzeTarget is the one relation an executor ANALYZE names ("schema.table"
// or "table", lower case), or "" for anything else (a column list is
// allowed; several relations, a second statement or a comment are not).
func analyzeTarget(sql string) string {
	s := strings.TrimSpace(sql)
	s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	word, rest := leadingWord(s)
	if !strings.EqualFold(word, "ANALYZE") || strings.Contains(rest, ";") {
		return ""
	}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "(") {
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			return ""
		}
		rest = strings.TrimSpace(rest[end+1:])
	} else if w, r := leadingWord(rest); strings.EqualFold(w, "VERBOSE") {
		rest = strings.TrimSpace(r)
	}
	parts, tail, ok := identifierParts(rest)
	tail = strings.TrimSpace(tail)
	if !ok || len(parts) > 2 || (tail != "" && !strings.HasPrefix(tail, "(")) {
		return ""
	}
	return strings.ToLower(strings.Join(parts, "."))
}

// leadingWord splits s after its leading run of letters.
func leadingWord(s string) (string, string) {
	i := 0
	for i < len(s) && unicode.IsLetter(rune(s[i])) {
		i++
	}
	return s[:i], s[i:]
}

// identifierParts reads a dotted identifier (each part plain or quoted)
// from the start of s and returns the parts and what follows.
func identifierParts(s string) ([]string, string, bool) {
	var parts []string
	for {
		part, rest, ok := identifierPart(s)
		if !ok {
			return nil, "", false
		}
		parts = append(parts, part)
		if !strings.HasPrefix(rest, ".") {
			return parts, rest, true
		}
		s = rest[1:]
	}
}

func identifierPart(s string) (string, string, bool) {
	if strings.HasPrefix(s, `"`) {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '"' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '"' {
				b.WriteByte('"')
				i++
				continue
			}
			return b.String(), s[i+1:], b.Len() > 0
		}
		return "", "", false
	}
	i := 0
	for i < len(s) && (s[i] == '_' || s[i] == '$' || s[i] >= 0x80 ||
		unicode.IsLetter(rune(s[i])) || (i > 0 && unicode.IsDigit(rune(s[i])))) {
		i++
	}
	return s[:i], s[i:], i > 0
}
