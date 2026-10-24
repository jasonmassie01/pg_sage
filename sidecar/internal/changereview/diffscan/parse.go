// Package diffscan reads the unified diff of a Change Review: it parses
// files and hunks with the line numbers on both sides (the reasoner's
// added-line gate), classifies which changed files touch the database
// (migrations, *.sql, ORM schemas, models, repositories, DAOs and files
// whose added lines carry an ORM call or raw SQL), and redacts secrets
// deterministically before any model call and before storage. The CLI
// uses it to skip changes that touch no database code.
//
// A diff is untrusted input: malformed text yields what parses, never a
// panic or an error, and nothing here executes or fetches anything.
package diffscan

import (
	"regexp"
	"strconv"
	"strings"
)

// File statuses.
const (
	StatusAdded    = "added"
	StatusModified = "modified"
	StatusDeleted  = "deleted"
	StatusRenamed  = "renamed"
)

// Line is one hunk line: Op '+' (added), '-' (removed) or ' ' (context);
// Old and New are its line numbers on each side (0 when absent there).
type Line struct {
	Op   byte
	Old  int
	New  int
	Text string
}

// Hunk is one "@@ -a,b +c,d @@ section" block.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Section            string
	Lines              []Line
}

// File is one file of a diff. Path is the new path (the old one for a
// deleted file); OldPath is set when the file existed before.
type File struct {
	Path    string
	OldPath string
	Status  string
	Binary  bool
	Hunks   []Hunk
}

// Range is an inclusive range of new-side line numbers.
type Range struct{ From, To int }

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// maxLineNumber bounds hunk positions so arithmetic never overflows.
const maxLineNumber = 1 << 30

// parser is one Parse call's state.
type parser struct {
	files []File
	cur   *File
	hunk  *Hunk
	oldN  int // old-side lines left in the hunk
	newN  int // new-side lines left in the hunk
	oldAt int
	newAt int
}

// Parse reads a unified diff (git or plain "diff -u" output). CRLF line
// endings are accepted. Files without a path are dropped.
func Parse(diff string) []File {
	p := &parser{}
	lines := strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1] // the final newline ends the last line
	}
	for i := 0; i < len(lines); i++ {
		i += p.line(lines, i)
	}
	p.flush()
	return p.files
}

// line handles lines[i] and returns how many extra lines it consumed.
func (p *parser) line(lines []string, i int) int {
	l := lines[i]
	if p.hunk != nil && (p.oldN > 0 || p.newN > 0) && p.hunkLine(l) {
		return 0
	}
	p.hunk = nil
	switch {
	case strings.HasPrefix(l, "diff --git "):
		p.flush()
		p.cur = gitFile(l)
	case strings.HasPrefix(l, "--- ") && i+1 < len(lines) &&
		strings.HasPrefix(lines[i+1], "+++ "):
		p.headers(l[4:], lines[i+1][4:])
		return 1
	case strings.HasPrefix(l, "@@ "):
		p.startHunk(l)
	case p.cur != nil:
		p.meta(l)
	}
	return 0
}

// gitFile is the file a "diff --git a/x b/y" header names.
func gitFile(l string) *File {
	f := &File{Status: StatusModified}
	rest := strings.TrimPrefix(l, "diff --git ")
	if i := strings.Index(rest, " b/"); i >= 0 && strings.HasPrefix(rest, "a/") {
		f.OldPath, f.Path = rest[2:i], rest[i+3:]
	}
	return f
}

// meta reads git's extended header lines.
func (p *parser) meta(l string) {
	switch {
	case strings.HasPrefix(l, "new file mode"):
		p.cur.Status, p.cur.OldPath = StatusAdded, ""
	case strings.HasPrefix(l, "deleted file mode"):
		p.cur.Status = StatusDeleted
	case strings.HasPrefix(l, "rename from "):
		p.cur.OldPath = strings.TrimPrefix(l, "rename from ")
	case strings.HasPrefix(l, "rename to "):
		p.cur.Path = strings.TrimPrefix(l, "rename to ")
	case strings.HasPrefix(l, "Binary files ") || strings.HasPrefix(l, "GIT binary patch"):
		p.cur.Binary = true
	}
	if p.cur.OldPath != "" && p.cur.Path != "" && p.cur.OldPath != p.cur.Path &&
		p.cur.Status == StatusModified {
		p.cur.Status = StatusRenamed
	}
}

// headers reads a "--- old" / "+++ new" pair, which starts a file when
// no git header came first.
func (p *parser) headers(oldRaw, newRaw string) {
	oldPath, newPath := headerPath(oldRaw, "a/"), headerPath(newRaw, "b/")
	if p.cur == nil || len(p.cur.Hunks) > 0 {
		p.flush()
		p.cur = &File{Status: StatusModified}
	}
	f := p.cur
	switch {
	case oldPath == "/dev/null":
		f.Status, f.OldPath, f.Path = StatusAdded, "", newPath
	case newPath == "/dev/null":
		f.Status, f.OldPath, f.Path = StatusDeleted, oldPath, oldPath
	default:
		f.OldPath, f.Path = oldPath, newPath
		if oldPath != newPath && f.Status == StatusModified {
			f.Status = StatusRenamed
		}
	}
}

// headerPath is a ---/+++ path without timestamp, quotes or a/ b/ prefix.
func headerPath(raw, prefix string) string {
	p := strings.SplitN(raw, "\t", 2)[0]
	p = strings.Trim(strings.TrimSpace(p), `"`)
	if p == "/dev/null" {
		return p
	}
	return strings.TrimPrefix(p, prefix)
}

func (p *parser) startHunk(l string) {
	m := hunkHeader.FindStringSubmatch(l)
	if m == nil || p.cur == nil {
		return
	}
	nums := [4]int{}
	for i, s := range []string{m[1], m[2], m[3], m[4]} {
		nums[i] = 1
		if s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 || n > maxLineNumber {
				return
			}
			nums[i] = n
		}
	}
	p.cur.Hunks = append(p.cur.Hunks, Hunk{OldStart: nums[0], OldLines: nums[1],
		NewStart: nums[2], NewLines: nums[3], Section: strings.TrimSpace(m[5])})
	p.hunk = &p.cur.Hunks[len(p.cur.Hunks)-1]
	p.oldN, p.newN, p.oldAt, p.newAt = nums[1], nums[3], nums[0], nums[2]
}

// hunkLine adds one line to the open hunk; false when l is not a hunk
// line (the hunk ends).
func (p *parser) hunkLine(l string) bool {
	op, text := byte(' '), ""
	switch {
	case l == "":
	case l[0] == '\\':
		return true
	case l[0] == '+' || l[0] == '-' || l[0] == ' ':
		op, text = l[0], l[1:]
	default:
		return false
	}
	ln := Line{Op: op, Text: text}
	switch op {
	case '+':
		ln.New, p.newAt, p.newN = p.newAt, p.newAt+1, p.newN-1
	case '-':
		ln.Old, p.oldAt, p.oldN = p.oldAt, p.oldAt+1, p.oldN-1
	default:
		ln.Old, ln.New = p.oldAt, p.newAt
		p.oldAt, p.newAt, p.oldN, p.newN = p.oldAt+1, p.newAt+1, p.oldN-1, p.newN-1
	}
	if (op != '-' && ln.New < 1) || (op == '-' && ln.Old < 1) {
		return false
	}
	p.hunk.Lines = append(p.hunk.Lines, ln)
	return true
}

func (p *parser) flush() {
	if p.cur != nil && p.cur.Path != "" {
		p.files = append(p.files, *p.cur)
	}
	p.cur, p.hunk = nil, nil
}

// AddedRanges are the new-side ranges of the file's added lines.
func (f File) AddedRanges() []Range {
	var out []Range
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Op != '+' {
				continue
			}
			if n := len(out); n > 0 && out[n-1].To == l.New-1 {
				out[n-1].To = l.New
				continue
			}
			out = append(out, Range{From: l.New, To: l.New})
		}
	}
	return out
}

// NearAdded reports whether new-side line is within tolerance lines of an
// added line.
func (f File) NearAdded(line, tolerance int) bool {
	if line < 1 || tolerance < 0 {
		return false
	}
	for _, r := range f.AddedRanges() {
		if line >= r.From-tolerance && line <= r.To+tolerance {
			return true
		}
	}
	return false
}

// AddedText is the file's added lines, one per line.
func (f File) AddedText() string {
	var b strings.Builder
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Op == '+' {
				b.WriteString(l.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// Find is the file at path: exact, or after dropping a "./", "a/" or "b/"
// prefix the model or a tool may add.
func Find(files []File, path string) (File, bool) {
	path = strings.TrimSpace(path)
	for _, candidate := range []string{path, strings.TrimPrefix(path, "./"),
		strings.TrimPrefix(path, "a/"), strings.TrimPrefix(path, "b/")} {
		if candidate == "" {
			continue
		}
		for _, f := range files {
			if f.Path == candidate {
				return f, true
			}
		}
	}
	return File{}, false
}
