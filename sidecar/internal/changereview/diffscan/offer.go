package diffscan

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Excerpt bounds.
const MaxExcerptLines = 400

// Excerpt errors.
var (
	ErrInvalidRange = errors.New("invalid diff excerpt range")
	ErrNoLines      = errors.New("no diff lines in the range")
)

// OfferedFile is one data-access file as the model is shown it: its
// rendered, redacted hunks.
type OfferedFile struct {
	Path  string
	Class Class
	Text  string
	Bytes int
}

// Offered is what a review offers the model: whole data-access files in
// offering order while they fit maxBytes, and the data-access files cut
// for size.
type Offered struct {
	Files []OfferedFile
	Cut   []string
	Bytes int
}

// kindRank orders offering: migrations and schemas, then models,
// repositories and DAOs, then other files with ORM calls.
func kindRank(kind string) int {
	switch kind {
	case KindSQL, KindMigration, KindSchema:
		return 0
	case KindModel, KindRepository, KindDAO:
		return 1
	}
	return 2
}

// Offer picks the data-access files of files to show the model, at most
// maxBytes of rendered text; a file that does not fit is cut whole and
// later smaller files may still fit.
func Offer(files []File, maxBytes int) Offered {
	type candidate struct {
		f File
		c Class
	}
	var cands []candidate
	for _, f := range files {
		if c := Classify(f); c.DataAccess {
			cands = append(cands, candidate{f, c})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return kindRank(cands[i].c.Kind) < kindRank(cands[j].c.Kind)
	})
	var out Offered
	for _, c := range cands {
		text := RenderFile(c.f)
		if out.Bytes+len(text) > maxBytes {
			out.Cut = append(out.Cut, c.f.Path)
			continue
		}
		out.Files = append(out.Files, OfferedFile{Path: c.f.Path, Class: c.c, Text: text,
			Bytes: len(text)})
		out.Bytes += len(text)
	}
	return out
}

// RenderFile is a file's hunks with new-side line numbers, redacted:
//
//	path (status)
//	@@ -a,b +c,d @@ section
//	  12 + added line
//	  13   context line
//	     - removed line
func RenderFile(f File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n", f.Path, f.Status)
	for _, h := range f.Hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		if h.Section != "" {
			b.WriteString(" " + h.Section)
		}
		b.WriteByte('\n')
		for _, l := range h.Lines {
			switch l.Op {
			case '-':
				fmt.Fprintf(&b, "     - %s\n", l.Text)
			default:
				fmt.Fprintf(&b, "%4d %c %s\n", l.New, l.Op, l.Text)
			}
		}
	}
	return Redact(b.String())
}

// Excerpt is the new-side lines from..to of f ("12 + text" for an added
// line, "13   text" for context), redacted and cut at a line boundary to
// at most maxBytes.
func Excerpt(f File, from, to, maxBytes int) (string, error) {
	if from < 1 || to < from || to-from >= MaxExcerptLines || maxBytes < 1 {
		return "", fmt.Errorf("%w: lines %d-%d (at most %d lines), %d bytes", ErrInvalidRange,
			from, to, MaxExcerptLines, maxBytes)
	}
	var b strings.Builder
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Op != '-' && l.New >= from && l.New <= to {
				fmt.Fprintf(&b, "%d %c %s\n", l.New, l.Op, l.Text)
			}
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%w: lines %d-%d of %s", ErrNoLines, from, to, f.Path)
	}
	return clipLines(Redact(b.String()), maxBytes), nil
}

// clipLines keeps whole lines of s within maxBytes; a first line longer
// than that is cut at a rune boundary.
func clipLines(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := strings.LastIndexByte(s[:maxBytes], '\n')
	if cut >= 0 {
		return s[:cut+1]
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// HunkFilter is the redacted changed lines of a diff's data-access files,
// by path, without line numbers or context: the DB-relevant part of a
// change for its content hash, stable when a rebase only shifts lines.
func HunkFilter(diff string) string {
	files := Parse(diff)
	sort.SliceStable(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var b strings.Builder
	for _, f := range files {
		if !Classify(f).DataAccess {
			continue
		}
		b.WriteString(f.Path + "\n")
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.Op != ' ' {
					b.WriteByte(l.Op)
					b.WriteString(l.Text + "\n")
				}
			}
		}
	}
	return Redact(b.String())
}
