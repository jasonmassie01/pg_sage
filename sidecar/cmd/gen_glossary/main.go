// Command gen_glossary writes web/src/generated/glossary.json from
// docs/glossary.md (ux-06a): one entry per term with its definition, the
// internal names it maps to and the words to avoid in text a person reads.
// The dashboard's Term tooltips and its retired-term test read the JSON. It is
// a build tool, not part of the shipped sidecar binary. Run it from sidecar/:
// go run ./cmd/gen_glossary/
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultDoc = "../docs/glossary.md"
	defaultOut = "web/src/generated/glossary.json"
	// maxDefinition is the longest definition, in characters, a tooltip shows.
	maxDefinition = 280
)

// Entry is one glossary term. ID is the term in lower case with spaces as
// hyphens; Line is where its heading is in the doc.
type Entry struct {
	ID         string   `json:"-"`
	Term       string   `json:"term"`
	Group      string   `json:"group"`
	Definition string   `json:"definition"`
	Internal   []string `json:"internal"`
	Avoid      []string `json:"avoid"`
	Line       int      `json:"-"`
	// avoidLine is the doc line of each avoided word.
	avoidLine map[string]int
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gen_glossary:", err)
		os.Exit(1)
	}
}

// run writes the JSON, or with -check fails when the file on disk differs
// from what the doc generates. A doc that does not parse writes nothing.
func run(args []string) error {
	fs := flag.NewFlagSet("gen_glossary", flag.ContinueOnError)
	doc := fs.String("doc", defaultDoc, "glossary Markdown (relative to sidecar/)")
	out := fs.String("out", defaultOut, "generated JSON path (relative to sidecar/)")
	check := fs.Bool("check", false, "write nothing; fail if the JSON is stale")
	if err := fs.Parse(args); err != nil {
		return err
	}
	entries, err := readDoc(*doc)
	if err != nil {
		return err
	}
	want, err := Render(entries)
	if err != nil {
		return err
	}
	if *check {
		return checkFile(*out, want)
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return fmt.Errorf("create the directory of %s: %w", *out, err)
	}
	if err := os.WriteFile(*out, want, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(os.Stderr, "gen_glossary: wrote %d terms to %s\n", len(entries), *out)
	return nil
}

func readDoc(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read glossary %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return Parse(f, filepath.Base(path))
}

// checkFile fails when path does not hold want (line endings aside).
func checkFile(path string, want []byte) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
		return fmt.Errorf("%s is stale against the glossary doc", path)
	}
	return nil
}

// Render writes the entries as JSON keyed by ID, keys sorted, with a final
// newline. No entries render as {}.
func Render(entries []Entry) ([]byte, error) {
	byID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	buf, err := json.MarshalIndent(byID, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode glossary JSON: %w", err)
	}
	return append(buf, '\n'), nil
}

// Parse reads the glossary Markdown. A "## " heading names the group of the
// entries after it; a "### " heading starts an entry, followed by one
// definition line and optional "- Internal names:" and "- Avoid:" lines.
// Every error names the file and line.
func Parse(r io.Reader, name string) ([]Entry, error) {
	p := &parser{name: name, seen: map[string]int{}}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		p.line++
		if err := p.feed(strings.TrimRight(sc.Text(), "\r \t")); err != nil {
			return nil, err
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if err := p.finish(); err != nil {
		return nil, err
	}
	return p.entries, checkAvoid(name, p.entries)
}
