package migration

import "github.com/pg-sage/sidecar/internal/migration/ddlrules"

// ruleDefinition describes a single DDL safety rule used by the
// RegexClassifier; the catalog lives in ddlrules so the case preflight
// reads the same lock table (migration-01).
type ruleDefinition = ddlrules.Rule

// ruleCatalog returns the rule definitions.
func ruleCatalog() []ruleDefinition { return ddlrules.Catalog() }
