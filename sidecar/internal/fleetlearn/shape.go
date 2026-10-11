// Package fleetlearn is pg_sage's fleet learning (roadmap phase 3): schema
// fingerprints per fleet database, look-alike priors drawn from the verified
// outcomes of databases that resemble each other, fleet findings (the same
// problem open on many databases) and the need measure behind the fleet's
// LLM budget split.
//
// Fingerprints are privacy-preserving: they hold only hashes of shapes
// (column type lists, index shapes, query shapes with every literal and
// identifier replaced), never data, literals or names unless the operator
// opts in. Priors never cross a fleet (control database) or tenant
// boundary, and they are evidence only: they never raise confidence, trust
// or authority.
package fleetlearn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// shapeHash is the first 16 hex characters of a shape's SHA-256.
func shapeHash(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:16]
}

// TableShapeHash is the shape of a table: its column types in column
// order, without names. A table without columns has no shape ("").
func TableShapeHash(columnTypes []string) string {
	if len(columnTypes) == 0 {
		return ""
	}
	return shapeHash("t1:" + strings.Join(columnTypes, ","))
}

// IndexShapeHash is the shape of an index: its table's shape, access
// method, uniqueness, partial flag and key column positions (0 for an
// expression). An index of a shapeless table has no shape.
func IndexShapeHash(tableShape, method string, unique, partial bool, keys []int) string {
	if tableShape == "" {
		return ""
	}
	return shapeHash(fmt.Sprintf("i1:%s:%s:%t:%t:%v", tableShape, method, unique,
		partial, keys))
}

// QueryShapeHash is the shape hash of a statement's normalized text; ""
// when the statement normalizes to nothing.
func QueryShapeHash(sql string) string {
	n := NormalizeQuery(sql)
	if n == "" {
		return ""
	}
	return shapeHash("q1:" + n)
}

// Boundary is the sharing boundary of a fleet database: priors only flow
// between databases of the same boundary. A database tagged tenant=<x>
// belongs to that tenant; every other database shares the operator's
// boundary "".
func Boundary(tags []string) string {
	for _, tag := range tags {
		key, value, ok := cutTag(tag)
		if ok && strings.EqualFold(key, "tenant") && value != "" {
			return "tenant:" + strings.ToLower(value)
		}
	}
	return ""
}

func cutTag(tag string) (string, string, bool) {
	i := strings.IndexAny(tag, "=:")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(tag[:i]), strings.TrimSpace(tag[i+1:]), true
}

var (
	identPart = `(?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$]*)`
	qualified = identPart + `(?:\s*\.\s*` + identPart + `)?`
	indexOnRe = regexp.MustCompile(`(?i)\bindex\b.*?\bon\s+(?:only\s+)?(` + qualified + `)`)
	alterRe   = regexp.MustCompile(`(?i)^\s*alter\s+table\s+(?:if\s+exists\s+)?` +
		`(?:only\s+)?(` + qualified + `)`)
	maintainRe = regexp.MustCompile(`(?i)^\s*(?:vacuum|analyze)\s*(?:\([^)]*\)\s*)?` +
		`(` + qualified + `)`)
)

// TableOfOutcome is the table an action acted on, as "schema.table" in
// lower case: from the finding's detail table, else its object identifier,
// else the executed statement. "" when none names a table.
func TableOfOutcome(detailTable, objectIdentifier, sql string) string {
	if t := strings.TrimSpace(detailTable); t != "" {
		return normalizeTable(t)
	}
	if obj := strings.TrimSpace(objectIdentifier); obj != "" {
		if i := strings.IndexAny(obj, "|("); i >= 0 {
			obj = obj[:i]
		}
		if obj = strings.TrimSpace(obj); obj != "" {
			return normalizeTable(obj)
		}
	}
	for _, re := range []*regexp.Regexp{indexOnRe, alterRe, maintainRe} {
		if m := re.FindStringSubmatch(sql); m != nil {
			return normalizeTable(m[1])
		}
	}
	return ""
}

// normalizeTable lower-cases a possibly quoted, possibly unqualified table
// name and qualifies it with public.
func normalizeTable(name string) string {
	name = strings.ToLower(strings.ReplaceAll(name, `"`, ""))
	name = strings.Join(strings.Fields(name), "")
	if !strings.Contains(name, ".") {
		return "public." + name
	}
	return name
}
