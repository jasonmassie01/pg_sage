package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// parser holds the state of one Parse: the entry being read and the line
// of each term seen so far.
type parser struct {
	name      string
	line      int
	group     string
	cur       *Entry
	defLine   int
	avoidLine map[string]int
	lists     map[string]bool
	seen      map[string]int
	entries   []Entry
}

func (p *parser) errorf(line int, format string, args ...any) error {
	return fmt.Errorf("%s:%d: %s", p.name, line, fmt.Sprintf(format, args...))
}

func (p *parser) malformed(what string) error {
	return p.errorf(p.line, "malformed entry %q: %s", p.cur.Term, what)
}

func heading(line, marks string) (string, bool) {
	if line == marks || strings.HasPrefix(line, marks+" ") {
		return strings.TrimSpace(strings.TrimPrefix(line, marks)), true
	}
	return "", false
}

// feed reads one line, trailing space already removed.
func (p *parser) feed(line string) error {
	if term, ok := heading(line, "###"); ok {
		if err := p.finish(); err != nil {
			return err
		}
		return p.start(term)
	}
	if group, ok := heading(line, "##"); ok {
		p.group = group
		return p.finish()
	}
	if p.cur == nil || line == "" {
		return nil
	}
	if strings.HasPrefix(line, "- ") {
		return p.list(strings.TrimPrefix(line, "- "))
	}
	if strings.HasPrefix(line, "#") {
		return p.malformed("unknown line")
	}
	if p.cur.Definition != "" {
		return p.malformed("the definition must be one line")
	}
	p.cur.Definition, p.defLine = strings.TrimSpace(line), p.line
	return nil
}

func (p *parser) start(term string) error {
	if term == "" {
		return p.errorf(p.line, "malformed entry: empty term")
	}
	id := strings.ToLower(strings.Join(strings.Fields(term), "-"))
	if first, dup := p.seen[id]; dup {
		return p.errorf(p.line, "duplicate term %q (first at line %d)", term, first)
	}
	p.seen[id] = p.line
	p.cur = &Entry{ID: id, Term: term, Group: p.group, Line: p.line,
		Internal: []string{}, Avoid: []string{}}
	p.lists, p.avoidLine = map[string]bool{}, map[string]int{}
	return nil
}

// list reads a "Key: a, b" line of the current entry.
func (p *parser) list(body string) error {
	key, values, ok := strings.Cut(body, ":")
	if !ok || (key != "Internal names" && key != "Avoid") {
		return p.malformed("unknown line")
	}
	if p.cur.Definition == "" {
		return p.malformed("the definition must come first")
	}
	if p.lists[key] {
		return p.malformed(key + " given twice")
	}
	p.lists[key] = true
	for _, v := range strings.Split(values, ",") {
		v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), "`"))
		if v == "" {
			return p.malformed("empty value in " + key)
		}
		if key == "Avoid" {
			v = strings.ToLower(v)
			p.cur.Avoid = append(p.cur.Avoid, v)
			p.avoidLine[v] = p.line
		} else {
			p.cur.Internal = append(p.cur.Internal, v)
		}
	}
	return nil
}

// finish closes the current entry, checking its definition.
func (p *parser) finish() error {
	e := p.cur
	if e == nil {
		return nil
	}
	p.cur = nil
	if e.Definition == "" {
		return p.errorf(e.Line, "term %q has no definition", e.Term)
	}
	if n := utf8.RuneCountInString(e.Definition); n > maxDefinition {
		return p.errorf(p.defLine, "definition of %q is %d characters; the limit is %d",
			e.Term, n, maxDefinition)
	}
	e.avoidLine = p.avoidLine
	p.entries = append(p.entries, *e)
	return nil
}

// checkAvoid fails when an avoided word is itself a term, or when two
// entries avoid the same word: the retired-term test could not say which
// term to use instead.
func checkAvoid(name string, entries []Entry) error {
	terms := map[string]bool{}
	for _, e := range entries {
		terms[strings.ToLower(e.Term)] = true
	}
	owner := map[string]string{}
	for _, e := range entries {
		for _, w := range e.Avoid {
			line := e.avoidLine[w]
			if terms[w] {
				return fmt.Errorf("%s:%d: avoid word %q of %q is itself a glossary term",
					name, line, w, e.Term)
			}
			if first, dup := owner[w]; dup {
				return fmt.Errorf("%s:%d: avoid word %q of %q is already avoided by %q",
					name, line, w, e.Term, first)
			}
			owner[w] = e.Term
		}
	}
	return nil
}
