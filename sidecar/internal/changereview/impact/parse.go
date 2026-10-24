package impact

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/sqlast"
)

// textFile is one file analyzed in text mode, and why.
type textFile struct {
	path   string
	reason string
	stmts  []*stmt
}

// parseFiles is stage 1: every file through libpg_query, or in text mode
// through the regex classifier when the parser is not compiled in or
// rejects the file (one parse error rejects the whole file).
func (r *run) parseFiles(files []changereview.SQLFile) []textFile {
	var texts []textFile
	for _, f := range files {
		if strings.TrimSpace(f.Content) == "" {
			continue
		}
		if !astAvailable() {
			texts = r.addText(texts, f, "the SQL parser (libpg_query) is not compiled "+
				"into this build")
			continue
		}
		parts, err := inspectDDL(f.Content)
		if err != nil {
			texts = r.addText(texts, f, parseReason(err))
			continue
		}
		r.addParsed(f.Path, parts)
	}
	return texts
}

func parseReason(err error) string {
	var pe *sqlast.ParseError
	if errors.As(err, &pe) {
		if pe.Line > 0 {
			return fmt.Sprintf("the SQL parser rejected line %d: %s", pe.Line, pe.Message)
		}
		return "the SQL parser rejected it: " + pe.Message
	}
	return "the SQL parser rejected it: " + err.Error()
}

// addParsed groups a file's parts into statements.
func (r *run) addParsed(path string, parts []sqlast.DDLStatement) {
	var cur *stmt
	for _, p := range parts {
		if cur == nil || p.Statement != cur.parts[0].Statement {
			cur = &stmt{index: len(r.stmts) + 1, file: path, line: p.Line, sql: p.Text}
			cur.evID = stmtEvidenceID(cur.index)
			if fp, err := sqlast.Fingerprint(p.Text); err == nil {
				cur.fingerprint = fp
			}
			r.stmts = append(r.stmts, cur)
		}
		cur.parts = append(cur.parts, p)
	}
	for _, s := range r.stmts {
		if s.kind == "" {
			s.kind = statementKind(s.parts)
		}
	}
}

// statementKind is one statement's kind: its parts' kind, "alter_table"
// for a mix, and the command for other statements ("select").
func statementKind(parts []sqlast.DDLStatement) string {
	kind := string(parts[0].Kind)
	for _, p := range parts[1:] {
		if string(p.Kind) != kind {
			return "alter_table"
		}
	}
	if parts[0].Kind == sqlast.DDLOther {
		return commandKind(parts[0].Command)
	}
	return kind
}

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// commandKind turns a parse node name into a kind: "SelectStmt" is
// "select", "CreateFunctionStmt" "create_function".
func commandKind(command string) string {
	c := strings.TrimSuffix(command, "Stmt")
	if c == "" {
		return string(sqlast.DDLOther)
	}
	return strings.ToLower(camelBoundary.ReplaceAllString(c, "${1}_${2}"))
}

// addText adds a file in text mode, unless it holds no statement (only
// comments).
func (r *run) addText(texts []textFile, f changereview.SQLFile, reason string) []textFile {
	if tf := r.parseText(f, reason); len(tf.stmts) > 0 {
		r.mode = changereview.ParseText
		return append(texts, tf)
	}
	return texts
}

// parseText is a file in text mode: its statements as the classifier
// splits them, each with the classifications that matched it.
func (r *run) parseText(f changereview.SQLFile, reason string) textFile {
	tf := textFile{path: f.Path, reason: reason}
	byText := map[string]*stmt{}
	for _, raw := range migration.SplitStatements(f.Content) {
		s := &stmt{index: len(r.stmts) + 1, file: f.Path, sql: collapse(raw),
			kind: "unparsed", text: true}
		s.evID = stmtEvidenceID(s.index)
		r.stmts = append(r.stmts, s)
		tf.stmts = append(tf.stmts, s)
		byText[s.sql] = s
	}
	for _, c := range migration.NewRegexClassifier().Classify(f.Content, r.version) {
		s := byText[c.Statement]
		if s == nil {
			continue
		}
		s.classes = append(s.classes, c)
		if c.TableName != "" {
			rel := qualified(sqlast.QualifiedName{Schema: c.SchemaName, Name: c.TableName})
			if !hasString(s.relations, rel) {
				s.relations = append(s.relations, rel)
			}
		}
	}
	return tf
}

var spaces = regexp.MustCompile(`\s+`)

func collapse(s string) string { return strings.TrimSpace(spaces.ReplaceAllString(s, " ")) }

// textModeFindings is one CR-18 warning per text-mode file.
func (r *run) textModeFindings(files []textFile) {
	for _, tf := range files {
		ids := make([]string, 0, len(tf.stmts))
		for _, s := range tf.stmts {
			ids = append(ids, s.evID)
		}
		name := tf.path
		if name == "" {
			name = "the inline SQL"
		}
		r.find(changereview.RuleTextMode, changereview.SeverityWarning,
			"SQL analyzed in text mode: "+name,
			fmt.Sprintf("%s: %s. pg_sage scored its DDL with the regex classifier (text "+
				"mode) and could not check it against the workload or the catalog; text "+
				"mode is never safe.", name, tf.reason), ids...)
		r.nd = append(r.nd, changereview.NotDetermined{Subject: name,
			Reason: "text mode: workload and catalog impact not analyzed"})
	}
}
