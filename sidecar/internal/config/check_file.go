package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// CheckFile backs `pg_sage_sidecar config validate` (cli-01b): it runs
// Load on one file offline (no database is opened) and, when Load refuses
// the file, walks the document against Config to name every unknown key,
// wrong type and out-of-range value (the config-02b registry) by its key
// path, not only the first. Load stays the verdict: the walk only names
// problems, so a file Load accepts is never reported invalid. A cross-key
// check (Validate) reports its first failure.

// FileProblem is one thing wrong with a config file: the dotted key it is
// at ("" for the file as a whole) and its line (0 when unknown).
type FileProblem struct {
	Key     string
	Line    int
	Message string
}

func (p FileProblem) String() string {
	where := p.Key
	if p.Line > 0 {
		where = strings.TrimSpace(fmt.Sprintf("%s (line %d)", p.Key, p.Line))
	}
	if where == "" {
		return p.Message
	}
	return where + ": " + p.Message
}

// FileCheck is CheckFile's answer. Config is the loaded configuration of
// a valid file; Problems is empty exactly when the file is valid.
type FileCheck struct {
	Config   *Config
	Problems []FileProblem
	Empty    bool // the file sets nothing: every setting is its default
}

// CheckFile validates the config file at path as the sidecar would load
// it (environment overlay and ${NAME} expansion included). The error is
// for a file that could not be read; it carries the OS error and path.
func CheckFile(path string) (FileCheck, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileCheck{}, fmt.Errorf("read the config file: %w", err)
	}
	raw := expandBracedEnv(string(data))
	cfg, loadErr := Load([]string{"--config", path})
	if loadErr == nil {
		return FileCheck{Config: cfg, Empty: documentIsEmpty(raw)}, nil
	}
	problems := keyProblems(raw)
	if !problemsCover(loadErr, problems) {
		problems = append(problems, problemFromError(loadErr, path))
	}
	return FileCheck{Problems: problems}, nil
}

// documentIsEmpty reports a document with no settings: no content, a
// null document or an empty mapping.
func documentIsEmpty(raw string) bool {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(raw), &doc) != nil {
		return false
	}
	if len(doc.Content) == 0 {
		return true
	}
	root := doc.Content[0]
	return isNullNode(root) || (root.Kind == yaml.MappingNode && len(root.Content) == 0)
}

// problemsCover reports whether the walk already names Load's error: a
// decode error (the walk re-reads every key) or a message naming a key
// the walk reported.
func problemsCover(loadErr error, problems []FileProblem) bool {
	if len(problems) == 0 {
		return false
	}
	msg := loadErr.Error()
	if strings.Contains(msg, "yaml: unmarshal errors") {
		return true
	}
	for _, p := range problems {
		if p.Key != "" && strings.Contains(msg, p.Key) {
			return true
		}
	}
	return false
}

// leadingKeyRe matches a dotted key at the start of a validation message.
var leadingKeyRe = regexp.MustCompile(`^([a-z][a-z0-9_]*(?:\.[a-z0-9_\[\]]+)+)\b`)

// problemFromError turns Load's error into one problem, naming the key
// its message starts with.
func problemFromError(err error, path string) FileProblem {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "loading config "+path+": ")
	msg = strings.TrimPrefix(msg, "config validation: ")
	key := leadingKeyRe.FindString(msg)
	return FileProblem{Key: key, Message: trimKey(msg, key)}
}

// trimKey drops a leading "key " from msg, which the key path already says.
func trimKey(msg, key string) string {
	if key != "" && strings.HasPrefix(msg, key+" ") {
		return strings.TrimPrefix(msg, key+" ")
	}
	return msg
}

// keyProblems walks the document against Config. A document that does
// not parse, or holds a retired section, has none: Load's error says so.
func keyProblems(raw string) []FileProblem {
	cleaned, err := dropRetired(raw)
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if yaml.Unmarshal([]byte(cleaned), &doc) != nil || len(doc.Content) == 0 {
		return nil
	}
	var w fileWalker
	w.walk(doc.Content[0], reflect.TypeOf(Config{}), "")
	return w.problems
}

var yamlUnmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()

// fileWalker collects the problems of one document.
type fileWalker struct {
	problems []FileProblem
}

func (w *fileWalker) add(key string, line int, msg string) {
	w.problems = append(w.problems, FileProblem{Key: key, Line: line,
		Message: trimKey(msg, key)})
}

// walk checks node against type t at the dotted key path.
func (w *fileWalker) walk(node *yaml.Node, t reflect.Type, path string) {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if isNullNode(node) {
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(yamlUnmarshalerType) {
		w.decode(node, t, path)
		return
	}
	switch {
	case t.Kind() == reflect.Struct && node.Kind == yaml.MappingNode:
		w.walkStruct(node, t, path)
	case t.Kind() == reflect.Map && node.Kind == yaml.MappingNode &&
		t.Key().Kind() == reflect.String:
		for i := 0; i+1 < len(node.Content); i += 2 {
			w.walk(node.Content[i+1], t.Elem(), joinKey(path, node.Content[i].Value))
		}
	case (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) &&
		node.Kind == yaml.SequenceNode:
		for i, item := range node.Content {
			w.walk(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	case t.Kind() == reflect.Struct, t.Kind() == reflect.Map, t.Kind() == reflect.Slice,
		t.Kind() == reflect.Array:
		w.decode(node, t, path) // the wrong shape: the decoder names it
	default:
		w.leaf(node, t, path)
	}
}

// walkStruct checks each key of a mapping against the struct's fields.
func (w *fileWalker) walkStruct(node *yaml.Node, t reflect.Type, path string) {
	fields := yamlFields(t)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		child := joinKey(path, key.Value)
		ft, ok := fields[key.Value]
		if !ok {
			w.add(child, key.Line, "unknown key")
			continue
		}
		w.walk(node.Content[i+1], ft, child)
	}
}

// leaf decodes a scalar and applies its registry range or enum.
func (w *fileWalker) leaf(node *yaml.Node, t reflect.Type, path string) {
	v, ok := w.decode(node, t, path)
	if !ok {
		return
	}
	spec, found := LookupKey(path)
	if !found {
		return
	}
	if err := spec.checkValue(v); err != nil {
		w.add(path, node.Line, err.Error())
	}
}

// decode decodes node into a new t, reporting a decode error at path.
func (w *fileWalker) decode(node *yaml.Node, t reflect.Type, path string) (reflect.Value, bool) {
	ptr := reflect.New(t)
	if err := node.Decode(ptr.Interface()); err != nil {
		w.add(path, node.Line, decodeMessage(err))
		return reflect.Value{}, false
	}
	return ptr.Elem(), true
}

// lineRe matches the "line N: " prefix of a yaml decode error.
var lineRe = regexp.MustCompile(`^line \d+: `)

// decodeMessage is a decode error without the yaml prefixes the line
// number already says.
func decodeMessage(err error) string {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return lineRe.ReplaceAllString(strings.TrimPrefix(err.Error(), "yaml: "), "")
	}
	parts := make([]string, 0, len(typeErr.Errors))
	for _, e := range typeErr.Errors {
		parts = append(parts, lineRe.ReplaceAllString(e, ""))
	}
	return strings.Join(parts, "; ")
}

// yamlFields maps each YAML key of struct t to its field type, as the
// yaml decoder names them. Config has no inline fields.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(sf.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(sf.Name)
		}
		out[name] = sf.Type
	}
	return out
}

func isNullNode(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

func joinKey(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
