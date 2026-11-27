package optimizer

import (
	"errors"
	"fmt"
	"strings"
)

// FuncRef is one function call in a CREATE INDEX's key list or WHERE
// predicate. ddl[Start:] begins with the name as written, qualifier
// included, so a schema can be inserted at Start.
type FuncRef struct {
	Schema string // "" when the call is unqualified
	Name   string // ASCII-folded unless quoted, as PostgreSQL reads it
	Start  int
}

func (r FuncRef) String() string {
	if r.Schema == "" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

// Grammar reports a bare call the SQL grammar binds to pg_catalog itself
// (EXTRACT(f FROM x), NORMALIZE(x, NFC), POSITION(a IN b), OVERLAY and
// SUBSTRING in their keyword forms). Naming a schema on one is a syntax
// error, so it is never qualified.
func (r FuncRef) Grammar() bool { return r.Schema == "" && grammarFunctions[r.Name] }

var grammarFunctions = map[string]bool{
	"extract": true, "normalize": true, "overlay": true, "position": true, "substring": true,
}

// ErrUnreadableExpression marks an index expression the call scanner will
// not read, so no call in it can be verified.
var ErrUnreadableExpression = errors.New("index expression cannot be read")

// operatorWords are keywords after which an operand starts, so a name
// followed by "(" there is a call. After any other name, a literal, a
// number or ")" an operand has ended, and a name followed by "(" is an
// infix keyword (email LIKE ('a%')) or an operator class with parameters.
var operatorWords = map[string]bool{
	"and": true, "or": true, "not": true, "is": true, "like": true, "ilike": true,
	"similar": true, "between": true, "in": true, "then": true, "when": true,
	"else": true, "case": true, "distinct": true, "from": true, "to": true,
	"escape": true, "symmetric": true, "for": true, "placing": true, "both": true,
	"leading": true, "trailing": true, "using": true, "collate": true, "at": true,
	"zone": true, "overlaps": true, "variadic": true, "asymmetric": true, "default": true,
}

// castTypeWords continue a multi-word type name after "::" or AS.
var castTypeWords = map[string]bool{
	"varying": true, "precision": true, "with": true, "without": true,
	"time": true, "zone": true,
}

// IndexFunctionCalls returns the function calls a CREATE INDEX makes in
// its key list and WHERE predicate, in order, with offsets in ddl. String
// literals, quoted identifiers, casts and infix keywords are read as
// PostgreSQL reads them. A statement the DDL scanner cannot read, and a
// comment, a dollar quote, a Unicode-escaped name or a backslash in a
// standard string literal (whose meaning depends on
// standard_conforming_strings), is an error, never a guess.
func IndexFunctionCalls(ddl string) ([]FuncRef, error) {
	refs, _, err := scanIndexExpressions(ddl)
	return refs, err
}

// scanIndexExpressions reads a CREATE INDEX's key list and predicate for
// IndexFunctionCalls and IndexObjectRefs.
func scanIndexExpressions(ddl string) ([]FuncRef, []ObjectRef, error) {
	s := &ddlScanner{src: strings.TrimRight(ddl, "; \t\r\n")}
	var spec IndexSpec
	if _, err := s.indexHeader(&spec); err != nil {
		return nil, nil, err
	}
	if err := s.parseBody(&spec); err != nil {
		return nil, nil, err
	}
	cs := &callScanner{src: s.src, pos: s.keysAt, end: s.keysEnd, keys: true}
	if err := cs.scan(); err != nil {
		return nil, nil, err
	}
	if spec.Where != "" {
		cs.pos, cs.end, cs.keys, cs.depth, cs.operand = s.whereAt, len(s.src), false, 0, false
		if err := cs.scan(); err != nil {
			return nil, nil, err
		}
	}
	return cs.refs, cs.objects, nil
}

// callScanner walks src[pos:end] token by token.
type callScanner struct {
	src      string
	pos, end int
	// operand is set when the last token ended an operand.
	operand bool
	// keys is set while the key list is read, and depth counts the open
	// parentheses and brackets: only a top-level key element is followed
	// by an operator class, the one qualified name before "(" that is not
	// a call.
	keys    bool
	depth   int
	refs    []FuncRef
	objects []ObjectRef
}

func (cs *callScanner) scan() error {
	for cs.pos < cs.end {
		if err := cs.step(); err != nil {
			return err
		}
	}
	return nil
}

func unreadable(what string) error {
	return fmt.Errorf("%w: %s", ErrUnreadableExpression, what)
}

func (cs *callScanner) peek(n int) byte {
	if cs.pos+n < cs.end {
		return cs.src[cs.pos+n]
	}
	return 0
}

func (cs *callScanner) step() error {
	c := cs.src[cs.pos]
	switch {
	case isPGSpaceByte(c):
		cs.pos++
	case c == '\'':
		return cs.literal(false)
	case c == '-' && cs.peek(1) == '-', c == '/' && cs.peek(1) == '*':
		return unreadable("a comment")
	case c == '$':
		return unreadable("a dollar-quoted string or parameter")
	case c >= '0' && c <= '9':
		for cs.pos < cs.end && (isIdentByte(cs.src[cs.pos]) || cs.src[cs.pos] == '.') {
			cs.pos++
		}
		cs.operand = true
	case c == '"' || isIdentByte(c):
		return cs.name()
	case c == ':' && cs.peek(1) == ':':
		cs.pos += 2
		return cs.castType()
	case c == '(' || c == '[':
		cs.pos++
		cs.depth++
		cs.operand = false
	case c == ')' || c == ']':
		cs.pos++
		cs.depth--
		cs.operand = true
	default:
		cs.pos++
		cs.operand = false
	}
	return nil
}

func isPGSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func (cs *callScanner) skipSpace() {
	for cs.pos < cs.end && isPGSpaceByte(cs.src[cs.pos]) {
		cs.pos++
	}
}

// literal skips a string literal; escape is set for E'...', where a
// backslash escapes the next character.
func (cs *callScanner) literal(escape bool) error {
	for i := cs.pos + 1; i < cs.end; i++ {
		switch cs.src[i] {
		case '\\':
			if !escape {
				return unreadable("a backslash in a standard string literal")
			}
			i++
		case '\'':
			if i+1 < cs.end && cs.src[i+1] == '\'' {
				i++
				continue
			}
			cs.pos, cs.operand = i+1, true
			return nil
		}
	}
	return unreadable("an unterminated string literal")
}

// namePart is one part of a dotted name.
type namePart struct {
	text   string
	quoted bool
}

// name reads a (possibly dotted) name and records it when it is a call.
func (cs *callScanner) name() error {
	start := cs.pos
	parts, err := cs.dottedName()
	if err != nil {
		return err
	}
	last := parts[len(parts)-1]
	single := len(parts) == 1 && !last.quoted
	if single && cs.pos < cs.end {
		switch next := cs.src[cs.pos]; {
		case next == '&' && last.text == "u":
			return unreadable("a Unicode-escaped name or string")
		case next == '\'' && cs.pos-start == 1:
			// E'..', B'..', X'..', N'..': a literal with a prefix.
			return cs.literal(last.text == "e")
		}
	}
	after := cs.pos
	cs.skipSpace()
	next := byte(0)
	if cs.pos < cs.end {
		next = cs.src[cs.pos]
	}
	if single && last.text == "operator" && next == '(' {
		return cs.operator()
	}
	cs.pos = after
	switch {
	case cs.isCall(parts, next):
		ref := FuncRef{Name: last.text, Start: start}
		if len(parts) > 1 {
			ref.Schema = parts[len(parts)-2].text
		}
		cs.refs = append(cs.refs, ref)
		cs.operand = false
	case len(parts) > 1 && next == '\'':
		// s.t 'v': a typed literal.
		cs.addObject(ObjectType, parts[:len(parts)-1], last.text)
		cs.operand = true
	case single && last.text == "as":
		return cs.castType()
	case single && (operatorWords[last.text] || sqlCallKeywords[last.text]):
		cs.operand = false
	default:
		cs.operand = true
	}
	return nil
}

// isCall reports whether the name just read, followed by next, is a call.
// After an operand a bare name before "(" is an infix keyword (email LIKE
// ('a%')); a qualified one is a call unless it is a top-level key
// element's operator class (col public.gist_trgm_ops(siglen=32)), so a
// keyword the scanner does not know never hides a qualified call.
func (cs *callScanner) isCall(parts []namePart, next byte) bool {
	last := parts[len(parts)-1]
	single := len(parts) == 1 && !last.quoted
	if next != '(' || (single && sqlCallKeywords[last.text]) {
		return false
	}
	if !cs.operand {
		return true
	}
	return len(parts) > 1 && (!cs.keys || cs.depth > 0)
}

// dottedName reads name [. name ...]; unquoted parts are ASCII-folded, as
// the PostgreSQL lexer folds them.
func (cs *callScanner) dottedName() ([]namePart, error) {
	var parts []namePart
	for {
		part, err := cs.namePart()
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		save := cs.pos
		cs.skipSpace()
		if cs.pos >= cs.end || cs.src[cs.pos] != '.' {
			cs.pos = save
			return parts, nil
		}
		cs.pos++
		cs.skipSpace()
		if cs.pos >= cs.end || (cs.src[cs.pos] != '"' && !isNameStart(cs.src[cs.pos])) {
			return parts, nil // t.* and the like
		}
	}
}

func isNameStart(c byte) bool {
	return isIdentByte(c) && (c < '0' || c > '9') && c != '$'
}

func (cs *callScanner) namePart() (namePart, error) {
	if cs.src[cs.pos] == '"' {
		var b strings.Builder
		for i := cs.pos + 1; i < cs.end; i++ {
			if cs.src[i] != '"' {
				b.WriteByte(cs.src[i])
				continue
			}
			if i+1 < cs.end && cs.src[i+1] == '"' {
				b.WriteByte('"')
				i++
				continue
			}
			cs.pos = i + 1
			return namePart{text: b.String(), quoted: true}, nil
		}
		return namePart{}, unreadable("an unterminated quoted name")
	}
	start := cs.pos
	for cs.pos < cs.end && isIdentByte(cs.src[cs.pos]) {
		cs.pos++
	}
	return namePart{text: asciiLower(cs.src[start:cs.pos])}, nil
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// castType skips the type name after "::" or AS, with its multi-word
// continuations (character varying, timestamp with time zone) and type
// modifiers, which are not calls.
func (cs *callScanner) castType() error {
	cs.skipSpace()
	if cs.pos >= cs.end || (cs.src[cs.pos] != '"' && !isNameStart(cs.src[cs.pos])) {
		return unreadable("a cast without a type name")
	}
	parts, err := cs.dottedName()
	if err != nil {
		return err
	}
	if len(parts) > 1 {
		cs.addObject(ObjectType, parts[:len(parts)-1], parts[len(parts)-1].text)
	}
	for {
		cs.skipSpace()
		if cs.pos >= cs.end {
			break
		}
		if cs.src[cs.pos] == '(' {
			if err := cs.skipParens(); err != nil {
				return err
			}
			continue
		}
		save := cs.pos
		if !isNameStart(cs.src[cs.pos]) {
			break
		}
		part, err := cs.namePart()
		if err != nil {
			return err
		}
		if !castTypeWords[part.text] {
			cs.pos = save
			break
		}
	}
	cs.operand = true
	return nil
}

// skipParens skips a balanced "( ... )" group (a type modifier).
func (cs *callScanner) skipParens() error {
	depth := 0
	for cs.pos < cs.end {
		switch cs.src[cs.pos] {
		case '\'':
			if err := cs.literal(false); err != nil {
				return err
			}
			continue
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				cs.pos++
				return nil
			}
		}
		cs.pos++
	}
	return unreadable("unbalanced parentheses")
}
