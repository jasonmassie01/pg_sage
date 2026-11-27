package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/pg-sage/sidecar/internal/migration"
)

// writeLintReport prints the review as JSON, GitHub workflow annotations
// or SARIF 2.1.0.
func writeLintReport(w io.Writer, format, file string, rep migration.Report) error {
	switch format {
	case "github":
		return writeGitHubAnnotations(w, file, rep)
	case "sarif":
		return writeSARIF(w, file, rep)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		File string `json:"file"`
		migration.Report
	}{File: file, Report: rep})
}

// lintFinding is one line of output: a hazard, or a statement that is
// unrecognized.
type lintFinding struct {
	rule, verdict, message string
	line                   int
}

func lintFindings(rep migration.Report) []lintFinding {
	var out []lintFinding
	for _, st := range rep.Statements {
		line := max(st.Line, 1)
		for _, h := range st.Hazards {
			msg := fmt.Sprintf("%s: %s (lock %s, table %s, size %s, risk %.2f).",
				h.Verdict, h.Description, h.LockLevel, h.Table, h.Size, h.RiskScore)
			if h.SafeAlternative != "" {
				msg += " Safer: " + h.SafeAlternative + "."
			}
			if st.GuardedRewrite != "" {
				msg += " Guarded rewrite:\n" + st.GuardedRewrite
			}
			out = append(out, lintFinding{h.RuleID, h.Verdict, msg, line})
		}
		if st.Status == migration.StatementUnrecognized {
			out = append(out, lintFinding{"unrecognized_statement", migration.VerdictReview,
				"review: " + st.Note, line})
		}
	}
	return out
}

// githubLevel maps a verdict to a workflow command.
func githubLevel(verdict string) string {
	switch verdict {
	case migration.VerdictRisky:
		return "error"
	case migration.VerdictReview:
		return "warning"
	}
	return "notice"
}

// githubEscape escapes workflow command data; property values also escape
// ':' and ','.
func githubEscape(s string, property bool) string {
	s = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
	if property {
		s = strings.NewReplacer(":", "%3A", ",", "%2C").Replace(s)
	}
	return s
}

func writeGitHubAnnotations(w io.Writer, file string, rep migration.Report) error {
	path := filepath.ToSlash(file)
	for _, f := range lintFindings(rep) {
		_, err := fmt.Fprintf(w, "::%s file=%s,line=%d,title=%s::%s\n", githubLevel(f.verdict),
			githubEscape(path, true), f.line, githubEscape("pg_sage "+f.rule, true),
			githubEscape(f.message, false))
		if err != nil {
			return err
		}
	}
	summary := fmt.Sprintf("pg_sage migration review: %s (%d statements, max risk %.2f). %s",
		rep.Verdict, len(rep.Statements), rep.MaxRiskScore, strings.Join(rep.Notes, " "))
	_, err := fmt.Fprintf(w, "::%s file=%s::%s\n", githubLevel(rep.Verdict),
		githubEscape(path, true), githubEscape(strings.TrimSpace(summary), false))
	return err
}
