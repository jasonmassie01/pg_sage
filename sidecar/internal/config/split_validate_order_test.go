package config

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// unsplitValidateOrder is every check of the unsplit validate() (config.go
// before config-30), in order: a call for a delegated check, the error text
// for an inline one. The order decides which error an operator sees first.
// A new check is added here where it runs.
var unsplitValidateOrder = []string{
	"c.validateTLS()",
	"c.API.validateTrustedOrigins()",
	"c.validateAgentNative()",
	"c.validateAuthz()",
	"c.validatePostgresPort()",
	"c.validateActionIdentity()",
	"collector.interval_seconds must be positive",
	"collector.batch_size must be positive",
	"collector.max_queries must be positive",
	"analyzer.interval_seconds must be positive",
	"analyzer.slow_query_threshold_ms must be non-negative",
	"analyzer.slow_query_min_calls must be non-negative, got %d",
	"c.Analyzer.validateSchemaGuard()",
	"c.Analyzer.validateTableBloat()",
	"c.Analyzer.validateIndexBloat()",
	"validateBudgets(c.Budgets)",
	"c.Briefing.validate()",
	"c.RCA.validate()",
	"c.SRE.validate()",
	"c.Ask.validate()",
	"c.Forecaster.validate()",
	"c.Facts.validate()",
	"c.validateRulePacks()",
	"c.Specialist.validate()",
	"c.LLM.validateWire()",
	"c.validateTrust()",
	"c.validateMigrationRehearsal()",
	"c.validateElevation()",
	"safety.cpu_ceiling_pct must be 1-100",
	"safety.query_timeout_ms must be positive",
	"c.Safety.validateClassTimeouts()",
	"c.validateIOAdmission()",
	"c.LakebaseObservability.resolveAndValidate(os.Getenv)",
	"c.validateCloudTelemetry()",
	"c.validateCost()",
	"c.Retention.validateSageFootprint()",
	"c.Analyzer.validateInactiveSlotMinMinutes()",
	"c.Analyzer.validateReplicationMinutes()",
	"c.Replication.validate()",
	"c.Analyzer.validateWraparoundFreezeShare()",
	"c.Analyzer.validateSelfCostBudget()",
	"c.SelfBudget.validate()",
	"c.Baseline.validate()",
	"c.validateHistory()",
	"c.LLM.Optimizer.RejectionMemory.validate()",
	"c.Tuning.validate()",
	"c.Tuner.validateParamShapeMinVariants()",
	"c.Tuner.validateHintCostTolerancePct()",
	"c.Retention.validateDecisionsDays()",
	"c.Retention.validateAuthAuditDays()",
	"c.Retention.validateStorage()",
	"c.SelfConfig.validate()",
	"c.validateFleetLearning()",
	"c.Fleet.Discovery.Validate()",
	"c.HA.validate()",
	"c.Backup.validate()",
	"c.Drill.validate()",
	"c.validateDrillCommand()",
	"c.Agents.validate()",
	"c.Audit.validate()",
	"c.MCP.OAuth.validate(c.MCP.Transport)",
	"validateReplicas(c.Databases)",
	"c.validateSLOOwners()",
	"c.Prometheus.validate()",
	"c.validateRegistry()",
	"c.validateFleetDatabases()",
}

// validateLeaves are checks in config_validate.go that are not groups.
var validateLeaves = map[string]bool{
	"validatePostgresPort": true, "validateTrust": true, "validateFleetDatabases": true,
}

// TestSplit_ValidateOrderMatchesUnsplit flattens validate()'s groups in
// config_validate.go and pins the order of every check, including the
// delegated ones the first-failure pairs cannot reach.
func TestSplit_ValidateOrderMatchesUnsplit(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config_validate.go", nil, 0)
	if err != nil {
		t.Fatalf("parse config_validate.go: %v", err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok {
			funcs[fd.Name.Name] = fd
		}
	}
	w := &orderWalker{t: t, fset: fset, funcs: funcs}
	w.group("validate")
	if strings.Join(w.got, "\n") != strings.Join(unsplitValidateOrder, "\n") {
		t.Fatalf("validate() check order changed:\n got %q\nwant %q",
			w.got, unsplitValidateOrder)
	}
}

type orderWalker struct {
	t     *testing.T
	fset  *token.FileSet
	funcs map[string]*ast.FuncDecl
	got   []string
}

// group records the checks of one group function in run order.
func (w *orderWalker) group(name string) {
	fd, ok := w.funcs[name]
	if !ok {
		w.t.Fatalf("group %s not found in config_validate.go", name)
	}
	for _, stmt := range fd.Body.List {
		switch s := stmt.(type) {
		case *ast.IfStmt:
			w.got = append(w.got, w.inlineError(s))
		case *ast.ReturnStmt:
			w.returned(s.Results[0])
		default:
			w.t.Fatalf("%s: unexpected statement %T", name, s)
		}
	}
}

// returned records a group's final firstError list or delegated call.
func (w *orderWalker) returned(expr ast.Expr) {
	if id, isIdent := expr.(*ast.Ident); isIdent && id.Name == "nil" {
		return
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		w.t.Fatalf("return of %T, want a call", expr)
	}
	if id, isIdent := call.Fun.(*ast.Ident); isIdent && id.Name == "firstError" {
		for _, arg := range call.Args {
			w.check(arg)
		}
		return
	}
	w.got = append(w.got, w.render(call))
}

// check records one firstError argument: a group is flattened, a method
// value or a closure is recorded as the call it makes.
func (w *orderWalker) check(arg ast.Expr) {
	switch a := arg.(type) {
	case *ast.SelectorExpr:
		if _, local := w.funcs[a.Sel.Name]; local && !validateLeaves[a.Sel.Name] {
			w.group(a.Sel.Name)
			return
		}
		w.got = append(w.got, w.render(a)+"()")
	case *ast.FuncLit:
		ret, ok := a.Body.List[0].(*ast.ReturnStmt)
		if len(a.Body.List) != 1 || !ok {
			w.t.Fatalf("closure is not a single return: %s", w.render(a))
		}
		w.got = append(w.got, w.render(ret.Results[0]))
	default:
		w.t.Fatalf("unexpected firstError argument %T", arg)
	}
}

// inlineError returns the error text of an inline `if ... { return err }`.
func (w *orderWalker) inlineError(s *ast.IfStmt) string {
	ret := s.Body.List[0].(*ast.ReturnStmt)
	call := ret.Results[0].(*ast.CallExpr)
	lit := call.Args[0].(*ast.BasicLit)
	text, err := strconv.Unquote(lit.Value)
	if err != nil {
		w.t.Fatalf("unquote %s: %v", lit.Value, err)
	}
	return text
}

func (w *orderWalker) render(node ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, w.fset, node); err != nil {
		w.t.Fatalf("render: %v", err)
	}
	return b.String()
}
