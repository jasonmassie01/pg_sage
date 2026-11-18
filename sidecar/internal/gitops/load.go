package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// subdirs are the directory's subdirectories with the kind each holds.
var subdirs = []struct{ dir, kind string }{
	{DirPolicySets, KindPolicySetFile},
	{DirPrincipals, KindPrincipalFile},
	{DirGrants, KindGrantsFile},
}

// rawFile is one YAML file read from the directory.
type rawFile struct {
	path, kind, hash string
	body             []byte
}

// loader collects the specs and every problem of one Load.
type loader struct {
	dir      *Directory
	problems []Problem
	sets     map[string]string // name → file
	agents   map[string]string
	grants   map[string]string // user|database → file
}

// Load reads and validates root's policy-sets/, principals/ and grants/
// YAML files (*.yaml or *.yml; other files and directories are ignored).
// A missing root is ErrDirectoryMissing; any invalid file is an
// *InvalidError listing every problem with its file and line.
func Load(root string) (*Directory, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("gitops: policy.directory is empty")
	}
	info, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrDirectoryMissing, root)
	}
	if err != nil {
		return nil, fmt.Errorf("gitops: reading policy.directory %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("gitops: policy.directory %s is not a directory", root)
	}
	files, problems, err := readFiles(root)
	if err != nil {
		return nil, err
	}
	l := &loader{dir: &Directory{Root: root, Hash: directoryHash(files)},
		problems: problems, sets: map[string]string{}, agents: map[string]string{},
		grants: map[string]string{}}
	for _, f := range files {
		l.parse(f)
	}
	if len(l.problems) > 0 {
		return nil, &InvalidError{Problems: l.problems}
	}
	return l.dir, nil
}

// readFiles reads every YAML file of the three subdirectories, sorted by
// path. A file over MaxFileBytes is a problem, not an error.
func readFiles(root string) ([]rawFile, []Problem, error) {
	var files []rawFile
	var problems []Problem
	for _, sub := range subdirs {
		entries, err := os.ReadDir(filepath.Join(root, sub.dir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("gitops: listing %s: %w", sub.dir, err)
		}
		for _, e := range entries {
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if e.IsDir() || (ext != ".yaml" && ext != ".yml") {
				continue
			}
			rel := sub.dir + "/" + e.Name()
			body, err := readBounded(filepath.Join(root, sub.dir, e.Name()))
			if errors.Is(err, errTooLarge) {
				problems = append(problems, Problem{File: rel,
					Message: "the file is over 1 MiB; split it"})
				continue
			}
			if err != nil {
				return nil, nil, fmt.Errorf("gitops: reading %s: %w", rel, err)
			}
			sum := sha256.Sum256(body)
			files = append(files, rawFile{path: rel, kind: sub.kind, body: body,
				hash: hex.EncodeToString(sum[:])})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	return files, problems, nil
}

var errTooLarge = errors.New("file over the size limit")

// readBounded reads at most MaxFileBytes, refusing one byte more.
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxFileBytes {
		return nil, errTooLarge
	}
	return body, nil
}

// directoryHash covers every file's path and content.
func directoryHash(files []rawFile) string {
	h := sha256.New()
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s\x00%s\n", f.path, f.hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// parse reads one file's root mapping, checks its kind and version and
// hands it to its kind's parser.
func (l *loader) parse(f rawFile) {
	var doc yaml.Node
	if err := yaml.Unmarshal(f.body, &doc); err != nil {
		l.add(Problem{File: f.path, Line: 1, Message: "the file is not YAML: " + err.Error()})
		return
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		l.add(Problem{File: f.path, Line: 1, Message: "the file is empty"})
		return
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		l.add(Problem{File: f.path, Line: root.Line, Message: "the file must be a mapping " +
			"with kind: and schema_version:"})
		return
	}
	m := newMapping(f, root)
	if !l.checkHeader(f, m) {
		return
	}
	switch f.kind {
	case KindPolicySetFile:
		l.parseSet(m)
	case KindPrincipalFile:
		l.parsePrincipal(m)
	case KindGrantsFile:
		l.parseGrants(m)
	}
}

// checkHeader requires the subdirectory's kind and schema_version 1.
func (l *loader) checkHeader(f rawFile, m mapping) bool {
	kind, ok := m.str("kind")
	switch {
	case !ok:
		l.add(Problem{File: f.path, Line: m.line, Message: fmt.Sprintf("kind: is required "+
			"(%s in %s/)", f.kind, dirOf(f.path))})
		return false
	case kind != f.kind:
		if dir := dirForKind(kind); dir != "" {
			l.add(m.problem("kind", "kind %s belongs in %s/, not %s/", kind, dir,
				dirOf(f.path)))
		} else {
			l.add(m.problem("kind", "unknown kind %q; %s/ holds %s files", kind,
				dirOf(f.path), f.kind))
		}
		return false
	}
	version, ok := m.integer("schema_version")
	if !ok || version != SchemaVersion {
		l.add(m.problem("schema_version", "schema_version must be %d", SchemaVersion))
		return false
	}
	return true
}

func dirOf(path string) string { return strings.SplitN(path, "/", 2)[0] }

func dirForKind(kind string) string {
	for _, s := range subdirs {
		if s.kind == kind {
			return s.dir
		}
	}
	return ""
}

func (l *loader) add(p Problem) { l.problems = append(l.problems, p) }

// claim records name for kind's file, or reports the earlier file.
func (l *loader) claim(seen map[string]string, key string, src Source, label string) bool {
	if first, ok := seen[key]; ok {
		l.add(Problem{File: src.Path, Line: src.Line,
			Message: fmt.Sprintf("%s is also in %s", label, first)})
		return false
	}
	seen[key] = src.Path
	return true
}
