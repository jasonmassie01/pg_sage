package optimizer

import (
	"regexp"
	"strings"
)

const (
	VectorShapeANN         = "ann_order_by_limit"
	VectorShapeFilteredANN = "filtered_ann"

	VectorWarningMissingLimit = "missing_limit"

	VectorRecommendationFilteredRecallCheck = "measure recall of the filtered ANN query " +
		"(pgvector iterative scans or a higher ef_search), with `pg_sage vector-lab`"
	VectorRecommendationAddLimit = "add_limit"
)

type VectorWorkloadClassification struct {
	Shape                 string
	PrimaryRecommendation string
	Warnings              []string
	Evidence              []string
}

// sqlIdent is one (lower-cased) SQL identifier, bare or double-quoted.
const sqlIdent = `(?:"[^"]+"|[a-z_][a-z0-9_$]*)`

// distanceOperand is an optionally qualified identifier, a $n parameter or
// a string literal, with an optional (schema-qualified, typmod) cast.
const distanceOperand = `('(?:[^']|'')*'|\$\d+|` + sqlIdent + `(?:\.` + sqlIdent + `)*)` +
	`(?:\s*::\s*(` + sqlIdent + `(?:\.` + sqlIdent + `)?)(?:\s*\(\s*\d+\s*\))?)?`

var (
	// vectorDistanceRe finds a <->, <=> or <#> with both operands. Groups:
	// left value, left cast, right value, right cast, a "(" after the right
	// value (a function call, not a column).
	vectorDistanceRe = regexp.MustCompile(distanceOperand +
		`\s*(?:<->|<=>|<#>)\s*` + distanceOperand + `(\s*\()?`)
	limitWordRe = regexp.MustCompile(`\blimit\b|\bfetch (?:first|next)\b`)
	whereWordRe = regexp.MustCompile(`\bwhere\b`)
	// literalOrCommentRe finds, left to right, a string literal or a
	// comment, so a "--" or "/*" inside a literal stays in the literal.
	literalOrCommentRe = regexp.MustCompile(`'(?:[^']|'')*'|--[^\n]*|(?s:/\*.*?\*/)`)
)

// vectorTypes are pgvector's types; a distance operator is a vector
// distance only when an operand has one of them.
var vectorTypes = map[string]bool{"vector": true, "halfvec": true, "sparsevec": true}

// ClassifyVectorWorkload classifies a pgvector ANN query. The distance
// operators are shared with PostGIS (<->, <#>), pg_trgm and btree_gist
// (<->), so a query is vector only when an operand is a column of type
// vector, halfvec or sparsevec in cols, or is cast to one. A column whose
// type is unknown is not vector: no hint rather than a wrong one.
func ClassifyVectorWorkload(q QueryInfo, cols []ColumnInfo) VectorWorkloadClassification {
	text := strings.Join(strings.Fields(blankLiteralsAndComments(
		strings.ToLower(q.Text))), " ")
	evidence := vectorOperands(text, cols)
	if len(evidence) == 0 {
		return VectorWorkloadClassification{}
	}

	out := VectorWorkloadClassification{
		Shape:    VectorShapeANN,
		Evidence: append([]string{"vector distance operator"}, evidence...),
	}
	if !limitWordRe.MatchString(text) {
		out.Warnings = append(out.Warnings, VectorWarningMissingLimit)
		out.PrimaryRecommendation = VectorRecommendationAddLimit
	}
	if whereWordRe.MatchString(text) {
		out.Shape = VectorShapeFilteredANN
		out.PrimaryRecommendation = VectorRecommendationFilteredRecallCheck
		out.Evidence = append(out.Evidence, "scalar filter before ANN ordering")
	}
	return out
}

// vectorOperands returns one evidence line per distance-operator operand
// that is vector-typed, by its cast or by its column's type.
func vectorOperands(text string, cols []ColumnInfo) []string {
	var out []string
	for _, m := range vectorDistanceRe.FindAllStringSubmatch(text, -1) {
		if ev := vectorOperand(m[1], m[2], false, cols); ev != "" {
			out = append(out, ev)
		}
		if ev := vectorOperand(m[3], m[4], m[5] != "", cols); ev != "" {
			out = append(out, ev)
		}
	}
	return out
}

// vectorOperand describes value (cast to castType when set) when it is
// vector-typed. A function call, literal or parameter without a vector
// cast is not.
func vectorOperand(value, castType string, call bool, cols []ColumnInfo) string {
	if castType != "" {
		if t := lastIdentPart(castType); vectorTypes[t] {
			return "cast to " + t
		}
		return ""
	}
	if call || strings.HasPrefix(value, "'") || strings.HasPrefix(value, "$") {
		return ""
	}
	name := lastIdentPart(value)
	for _, c := range cols {
		if strings.EqualFold(c.Name, name) && vectorTypes[strings.ToLower(c.Type)] {
			return name + " is " + strings.ToLower(c.Type)
		}
	}
	return ""
}

// lastIdentPart is the last part of a qualified identifier, unquoted.
func lastIdentPart(ident string) string {
	if i := strings.LastIndex(ident, "."); i >= 0 {
		ident = ident[i+1:]
	}
	return strings.Trim(ident, `"`)
}

// blankLiteralsAndComments empties every string literal (an empty literal
// keeps its cast an operand) and drops every comment, so neither an
// operator nor a LIMIT or WHERE written inside one is read as SQL.
func blankLiteralsAndComments(text string) string {
	return literalOrCommentRe.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasPrefix(m, "'") {
			return "''"
		}
		return " "
	})
}
