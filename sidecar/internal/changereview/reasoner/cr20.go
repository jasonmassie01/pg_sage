package reasoner

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
)

// CR-20 not_analyzable_without_sql: an ORM migration was changed, no SQL
// was submitted and no model-translated statement was accepted, so the
// deterministic engine had nothing to review. The finding gives the exact
// command that prints the SQL the tool would run.

// toolOfORM is the migration tool whose export command fits an ORM the
// classifier recognized.
var toolOfORM = map[string]string{diffscan.ORMActiveRecord: "rails",
	diffscan.ORMDjango: "django", diffscan.ORMSQLAlchemy: "alembic",
	diffscan.ORMPrisma: "prisma", diffscan.ORMKnex: "knex",
	diffscan.ORMSequelize: "sequelize", diffscan.ORMTypeORM: "typeorm"}

const maxNamedFiles = 5

// cr20 is the CR-20 finding and its evidence, when the review needs one.
func (s *session) cr20(accepted []changereview.Statement) (changereview.Finding,
	changereview.Evidence, bool) {
	if s.hasSQL() {
		return changereview.Finding{}, changereview.Evidence{}, false
	}
	for _, st := range accepted {
		if st.Origin == changereview.OriginModelTranslated {
			return changereview.Finding{}, changereview.Evidence{}, false
		}
	}
	var paths []string
	orm := ""
	for _, f := range s.files {
		if c := diffscan.Classify(f); c.MigrationCode && f.Status != diffscan.StatusDeleted {
			paths = append(paths, f.Path)
			if orm == "" {
				orm = c.ORM
			}
		}
	}
	if len(paths) == 0 {
		return changereview.Finding{}, changereview.Evidence{}, false
	}
	tool, command := exportCommand(s.in.MigrationTool, orm)
	ev, err := changereview.NewEvidence("migration_code", shortHash(paths...),
		"ORM migration code without SQL", map[string]any{"files": paths,
			"migration_tool": tool, "export_command": command})
	if err != nil {
		s.r.d.Logf("change review: review %s: CR-20 evidence: %v", s.in.ReviewID, err)
		return changereview.Finding{}, changereview.Evidence{}, false
	}
	f := changereview.Finding{RuleID: changereview.RuleNotAnalyzableSQL,
		Kind: "not_analyzable_without_sql", Severity: changereview.SeverityWarning,
		Origin: changereview.OriginRule, Title: "ORM migration without SQL: not analyzable",
		Text: fmt.Sprintf("pg_sage cannot review %s without the SQL it runs, and no SQL "+
			"was submitted. Print it with `%s` and submit that SQL (sql_files) for the "+
			"deterministic review.", namedFiles(paths), command),
		EvidenceIDs: []string{ev.ID}}
	return f, ev, true
}

func (s *session) hasSQL() bool {
	if len(s.a.Statements) > 0 {
		return true
	}
	for _, f := range s.in.SQLFiles {
		if strings.TrimSpace(f.Content) != "" {
			return true
		}
	}
	return false
}

// exportCommand is the declared tool's SQL export command, else the one of
// the tool the files' ORM implies, else a generic instruction.
func exportCommand(declared, orm string) (string, string) {
	for _, name := range []string{declared, toolOfORM[orm]} {
		if t, ok := changereview.LookupMigrationTool(name); ok && t.ExportCommand != "" {
			return t.Name, t.ExportCommand
		}
	}
	return declared, "the migration tool's command that prints the SQL it would run"
}

func namedFiles(paths []string) string {
	if len(paths) <= maxNamedFiles {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more files", strings.Join(paths[:maxNamedFiles], ", "),
		len(paths)-maxNamedFiles)
}
