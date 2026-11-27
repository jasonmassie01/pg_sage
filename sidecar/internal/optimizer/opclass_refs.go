package optimizer

import (
	"fmt"
	"strings"
)

// OpclassRef is an operator class a CREATE INDEX key names. ddl[Start:End]
// is the name as written, qualifier included.
type OpclassRef struct {
	Schema     string // "" when the name is unqualified
	Name       string // case-folded unless quoted, as PostgreSQL reads it
	Start, End int
}

// IndexOpclasses returns the access method of a CREATE INDEX statement
// (btree when USING is omitted) and the operator classes its key list
// names, in key order, with their offsets in ddl. The executor uses it to
// name an extension operator class's schema before the statement runs
// under its pinned search_path. A statement the DDL scanner cannot read is
// an error, never a guess.
func IndexOpclasses(ddl string) (string, []OpclassRef, error) {
	s := &ddlScanner{src: ddl}
	var spec IndexSpec
	if _, err := s.indexHeader(&spec); err != nil {
		return "", nil, err
	}
	method := "btree"
	if s.keyword("USING") {
		m, err := s.identifier()
		if err != nil {
			return "", nil, fmt.Errorf("index method: %w", err)
		}
		method = strings.ToLower(m)
	}
	s.skipSpace()
	listStart := s.pos + 1
	keys, err := s.parens()
	if err != nil {
		return "", nil, fmt.Errorf("key list: %w", err)
	}
	refs, err := keyOpclasses(keys)
	if err != nil {
		return "", nil, err
	}
	for i := range refs {
		refs[i].Start += listStart
		refs[i].End += listStart
	}
	return method, refs, nil
}

// keyOpclasses finds the operator class each key of a key list names, with
// offsets relative to list.
func keyOpclasses(list string) ([]OpclassRef, error) {
	spans, err := topLevelSpans(list)
	if err != nil {
		return nil, err
	}
	var refs []OpclassRef
	for _, sp := range spans {
		ks := &ddlScanner{src: list[sp[0]:sp[1]]}
		if _, err := ks.keyExpression(); err != nil {
			return nil, fmt.Errorf("index key %q: %w", ks.src, err)
		}
		if _, err := ks.keyDecorations(); err != nil {
			return nil, fmt.Errorf("index key %q: %w", ks.src, err)
		}
		start, end := ks.opclassAt[0], ks.opclassAt[1]
		if end <= start {
			continue
		}
		ref, err := opclassRefOf(ks.src[start:end])
		if err != nil {
			return nil, err
		}
		ref.Start, ref.End = sp[0]+start, sp[0]+end
		refs = append(refs, ref)
	}
	return refs, nil
}

// opclassRefOf splits an operator class name as written into its schema
// (the part before the name, when qualified) and its name.
func opclassRefOf(text string) (OpclassRef, error) {
	s := &ddlScanner{src: text}
	var parts []string
	for {
		id, err := s.identifier()
		if err != nil {
			return OpclassRef{}, fmt.Errorf("operator class %q: %w", text, err)
		}
		parts = append(parts, id)
		if !s.consume('.') {
			break
		}
	}
	ref := OpclassRef{Name: parts[len(parts)-1]}
	if len(parts) > 1 {
		ref.Schema = parts[len(parts)-2]
	}
	return ref, nil
}
