package executor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/verify"
)

// fleetpol-02b: the two typed exposure contracts, their statements, the
// target rule and the remediations an exposure finding yields.
//
// No concurrent access tests here: every function in this file is pure;
// the shared paths (apply lease, gate) are covered in exposure_db_test.go.

func TestExposureContractsAreRegistered(t *testing.T) {
	revoke, ok := ContractForActionType(ActionTypeRevokePublicGrant)
	if !ok || revoke.BaseRiskTier != "moderate" || revoke.RollbackClass != "reversible" {
		t.Fatalf("revoke_public_grant = %+v (registered %v), want moderate reversible",
			revoke, ok)
	}
	rls, ok := ContractForActionType(ActionTypeEnableRLS)
	if !ok || rls.BaseRiskTier != "high" || rls.RollbackClass != "reversible" {
		t.Fatalf("enable_row_level_security = %+v (registered %v), want high reversible",
			rls, ok)
	}
	if got := policyContract(rls).Guardrails; len(got) != 1 ||
		got[0] != policy.GuardrailApprovalRequired {
		t.Fatalf("RLS guardrails = %v, want approval_required", got)
	}
	if len(policyContract(revoke).Guardrails) != 0 {
		t.Fatal("revoke_public_grant may run automatically at a granted tier")
	}
	if revoke.Narrowing || rls.Narrowing {
		t.Fatal("exposure contracts are not narrowing: they can break a reader")
	}
	for _, c := range []ActionContract{revoke, rls} {
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", c.ActionType, err)
		}
		if got := changeClassForActionType(c.ActionType); got != string(policy.ChangeExposure) {
			t.Fatalf("%s change class = %q, want exposure", c.ActionType, got)
		}
	}
}

func TestExposureStatementsClassify(t *testing.T) {
	cases := map[string]string{
		`REVOKE SELECT ON TABLE public.leads FROM PUBLIC`:                ActionTypeRevokePublicGrant,
		`REVOKE ALL ON TABLE "App"."Lead s" FROM PUBLIC`:                 ActionTypeRevokePublicGrant,
		`revoke select, insert on public.leads from public;`:             ActionTypeRevokePublicGrant,
		`GRANT SELECT ON TABLE public.leads TO PUBLIC`:                   ActionTypeRevokePublicGrant,
		`ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY`:             ActionTypeEnableRLS,
		`ALTER TABLE "public"."leads" DISABLE ROW LEVEL SECURITY`:        ActionTypeEnableRLS,
		`ALTER TABLE public.leads SET (autovacuum_vacuum_scale_factor=0.1)`: "set_table_autovacuum",
	}
	for sql, want := range cases {
		if got := ActionTypeForSQL(sql); got != want {
			t.Errorf("ActionTypeForSQL(%q) = %q, want %q", sql, got, want)
		}
	}
	if got := categorizeAction(`REVOKE SELECT ON TABLE public.leads FROM PUBLIC`); got !=
		ActionTypeRevokePublicGrant {
		t.Errorf("categorizeAction(revoke) = %q", got)
	}
}

func TestExposureStatementsParse(t *testing.T) {
	s, ok := parseExposureSQL(`REVOKE select, Insert ON TABLE "App"."Lead s" FROM PUBLIC`)
	if !ok || s.kind != exposureRevoke || s.schema != "App" || s.table != "Lead s" {
		t.Fatalf("parse = %+v %v", s, ok)
	}
	if strings.Join(s.privileges, ",") != "INSERT,SELECT" {
		t.Fatalf("privileges = %v, want sorted INSERT,SELECT", s.privileges)
	}
	if got := s.qualified(); got != `"App"."Lead s"` {
		t.Fatalf("qualified = %s", got)
	}
	all, ok := parseExposureSQL(`REVOKE ALL PRIVILEGES ON public.leads FROM PUBLIC`)
	if !ok || len(all.privileges) != len(tablePrivileges) {
		t.Fatalf("ALL = %+v %v, want every table privilege", all, ok)
	}
}

// Invalid input: anything but the exact typed forms is not an exposure
// statement and the validator refuses it.
func TestExposureStatementsRefuseOtherForms(t *testing.T) {
	bad := []string{
		`REVOKE SELECT ON TABLE leads FROM PUBLIC`,                    // unqualified
		`REVOKE SELECT ON TABLE public.leads FROM app_reader`,         // not PUBLIC
		`REVOKE SELECT (email) ON TABLE public.leads FROM PUBLIC`,     // column list
		`REVOKE SELECT ON TABLE public.leads, public.b FROM PUBLIC`,    // two tables
		`REVOKE SELECT ON TABLE public.leads FROM PUBLIC CASCADE`,     // cascade
		`REVOKE GRANT OPTION FOR SELECT ON public.leads FROM PUBLIC`,  // option
		`REVOKE SELECT ON ALL TABLES IN SCHEMA public FROM PUBLIC`,     // bulk
		`GRANT SELECT ON TABLE public.leads TO app_reader`,            // not PUBLIC
		`GRANT SELECT ON TABLE public.leads TO PUBLIC WITH GRANT OPTION`,
		`REVOKE USAGE ON SCHEMA public FROM PUBLIC`,
		`ALTER TABLE public.leads FORCE ROW LEVEL SECURITY`,
		`ALTER TABLE leads ENABLE ROW LEVEL SECURITY`,
		`REVOKE SELECT ON TABLE public.leads FROM PUBLIC; DROP TABLE x`,
	}
	for _, sql := range bad {
		if s, ok := parseExposureSQL(sql); ok {
			t.Errorf("parseExposureSQL(%q) accepted %+v", sql, s)
		}
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Errorf("ValidateExecutorSQL(%q) accepted it", sql)
		}
	}
	for _, sql := range []string{`REVOKE SELECT ON TABLE pg_catalog.pg_class FROM PUBLIC`,
		`ALTER TABLE sage.findings ENABLE ROW LEVEL SECURITY`,
		`REVOKE SELECT ON TABLE information_schema.tables FROM PUBLIC`} {
		if err := ValidateExecutorSQL(sql); err == nil {
			t.Errorf("a system or sage table passed validation: %s", sql)
		}
	}
	for _, sql := range []string{`REVOKE SELECT ON TABLE public.leads FROM PUBLIC`,
		`GRANT SELECT, UPDATE ON TABLE public.leads TO PUBLIC`,
		`ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE public.leads DISABLE ROW LEVEL SECURITY`} {
		if err := ValidateExecutorSQL(sql); err != nil {
			t.Errorf("ValidateExecutorSQL(%q) = %v", sql, err)
		}
	}
}

// A re-grant or an RLS disable is never a forward action: it classifies to
// a contract only as the rollback of the matching forward statement.
func TestExposureInverseIsRollbackOnly(t *testing.T) {
	regrant := `GRANT SELECT ON TABLE public.leads TO PUBLIC`
	if _, ok := contractForFinding(analyzer.Finding{RecommendedSQL: regrant}); ok {
		t.Fatal("a forward re-grant must have no contract")
	}
	revoke := `REVOKE SELECT ON TABLE public.leads FROM PUBLIC`
	if c, ok := contractForFinding(analyzer.Finding{RecommendedSQL: revoke}); !ok ||
		c.ActionType != ActionTypeRevokePublicGrant {
		t.Fatalf("forward revoke contract = %+v %v", c, ok)
	}
	c, ok := rollbackContractFor(revoke, regrant)
	if !ok || c.ActionType != ActionTypeRevokePublicGrant {
		t.Fatalf("rollback contract of the revoke = %+v %v", c, ok)
	}
	if _, ok := rollbackContractFor(revoke,
		`GRANT SELECT ON TABLE public.other TO PUBLIC`); ok {
		t.Fatal("a re-grant on another table is not this revoke's rollback")
	}
	enable := `ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY`
	disable := `ALTER TABLE public.leads DISABLE ROW LEVEL SECURITY`
	if _, ok := contractForFinding(analyzer.Finding{RecommendedSQL: disable}); ok {
		t.Fatal("a forward RLS disable must have no contract")
	}
	if c, ok := rollbackContractFor(enable, disable); !ok || c.ActionType != ActionTypeEnableRLS {
		t.Fatalf("rollback contract of the RLS enable = %+v %v", c, ok)
	}
	if missing := unclassifiedRollbacks(forwardRollbackSamples); len(missing) != 0 {
		t.Fatalf("unclassified rollbacks: %v", missing)
	}
}

func exposureFinding(detector, objectType, object string) analyzer.Finding {
	return analyzer.Finding{Category: "agent_posture:" + detector, ObjectType: objectType,
		ObjectIdentifier: object, Title: detector + " exposure",
		Detail: map[string]any{"detector": detector, "mode": "shadow"}}
}

func TestExposureTargetRefusal(t *testing.T) {
	revoke, _ := parseExposureSQL(`REVOKE SELECT ON TABLE public.leads FROM PUBLIC`)
	ap15 := exposureFinding("AP-15", "schema", "public")
	ap15.Detail["evidence"] = []any{map[string]any{"source": "pg_class.relacl",
		"ref": "public.leads"}}
	if err := exposureTargetRefusal(ap15, revoke); err != nil {
		t.Fatalf("an AP-15 finding naming public.leads refused: %v", err)
	}
	rls, _ := parseExposureSQL(`ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY`)
	if err := exposureTargetRefusal(exposureFinding("AP-03", "table", "public.leads"),
		rls); err != nil {
		t.Fatalf("an AP-03 finding on public.leads refused: %v", err)
	}
	other, _ := parseExposureSQL(`REVOKE SELECT ON TABLE public.other FROM PUBLIC`)
	pgView, _ := parseExposureSQL(`REVOKE SELECT ON TABLE pg_catalog.pg_roles FROM PUBLIC`)
	sysTable, _ := parseExposureSQL(`REVOKE SELECT ON TABLE pg_catalog.pg_class FROM PUBLIC`)
	refused := map[string]struct {
		f    analyzer.Finding
		stmt exposureStmt
	}{
		"table not named":     {ap15, other},
		"pg_catalog view":     {exposureFinding("AP-15", "schema", "pg_catalog"), pgView},
		"system catalog":      {exposureFinding("AP-03", "table", "pg_catalog.pg_class"), sysTable},
		"not an exposure":     {exposureFinding("AP-04", "table", "public.leads"), rls},
		"revoke from AP-03":   {exposureFinding("AP-03", "table", "public.leads"), revoke},
		"RLS from AP-15":      {ap15, rls},
		"AP-03 on other":      {exposureFinding("AP-03", "table", "public.other"), rls},
		"empty finding":       {analyzer.Finding{}, revoke},
		"AP-15 no evidence":   {exposureFinding("AP-15", "schema", "public"), revoke},
		"inverse is forward":  {ap15, exposureStmt{kind: exposureRegrant, schema: "public", table: "leads"}},
	}
	for name, c := range refused {
		err := exposureTargetRefusal(c.f, c.stmt)
		if !errors.Is(err, ErrNotExposureTarget) ||
			!strings.Contains(err.Error(), "not an exposure finding target") {
			t.Errorf("%s: err = %v, want not an exposure finding target", name, err)
		}
	}
}

func TestExposureRemediationsFromFindings(t *testing.T) {
	ap15 := exposureFinding("AP-15", "schema", "public")
	ap15.Detail["evidence"] = []any{
		map[string]any{"source": "pg_class.relacl", "ref": "public.leads"},
		map[string]any{"source": "pg_class.relacl", "ref": `public."Odd ""name"""`},
		map[string]any{"source": "pg_default_acl", "ref": "x"},
		map[string]any{"source": "pg_class.relacl", "ref": "not qualified"},
	}
	got := exposureRemediations(ap15)
	if len(got) != 2 {
		t.Fatalf("AP-15 remediations = %+v, want two revokes", got)
	}
	if got[0].RecommendedSQL != `REVOKE ALL ON TABLE public.leads FROM PUBLIC` ||
		got[0].ObjectIdentifier != "public.leads" || got[0].Category != ap15.Category {
		t.Fatalf("first remediation = %+v", got[0])
	}
	if got[1].RecommendedSQL != `REVOKE ALL ON TABLE public."Odd ""name""" FROM PUBLIC` {
		t.Fatalf("quoted remediation = %q", got[1].RecommendedSQL)
	}
	for _, r := range got {
		if r.RollbackSQL != "" {
			t.Fatalf("the rollback is computed at run time, not proposed: %q", r.RollbackSQL)
		}
		if err := ValidateExecutorSQL(r.RecommendedSQL); err != nil {
			t.Fatalf("remediation SQL invalid: %v", err)
		}
	}
	rls := exposureRemediations(exposureFinding("AP-03", "table", "public.leads"))
	if len(rls) != 1 ||
		rls[0].RecommendedSQL != `ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY` ||
		rls[0].RollbackSQL != `ALTER TABLE public.leads DISABLE ROW LEVEL SECURITY` {
		t.Fatalf("AP-03 remediation = %+v", rls)
	}
	for name, f := range map[string]analyzer.Finding{
		"AP-03 view":   exposureFinding("AP-03", "view", "public.v"),
		"AP-03 schema": exposureFinding("AP-03", "schema", "public"),
		"AP-04":        exposureFinding("AP-04", "table", "public.leads"),
		"no evidence":  exposureFinding("AP-15", "schema", "public"),
		"zero":         {},
	} {
		if got := exposureRemediations(f); len(got) != 0 {
			t.Errorf("%s: remediations = %+v, want none", name, got)
		}
	}
}

func TestExposureRevokablePrivileges(t *testing.T) {
	cases := []struct {
		requested, held, want []string
	}{
		{tablePrivileges, []string{"SELECT", "UPDATE"}, []string{"SELECT", "UPDATE"}},
		{[]string{"SELECT"}, []string{"SELECT", "UPDATE"}, []string{"SELECT"}},
		{[]string{"DELETE"}, []string{"SELECT"}, nil},
		{tablePrivileges, nil, nil},
		{nil, []string{"SELECT"}, nil},
	}
	for _, c := range cases {
		got := revokablePrivileges(c.requested, c.held)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("revokable(%v, %v) = %v, want %v", c.requested, c.held, got, c.want)
		}
	}
	if got := regrantSQL("public", "leads", []string{"SELECT", "UPDATE"}); got !=
		`GRANT SELECT, UPDATE ON TABLE public.leads TO PUBLIC` {
		t.Fatalf("regrant = %q", got)
	}
}

func TestExposureRLSApprovalCard(t *testing.T) {
	var stmts []exposureStatement
	for i := range 25 {
		stmts = append(stmts, exposureStatement{QueryID: int64(100 + i), Calls: int64(50 - i)})
	}
	card := rlsApprovalCard("public", "leads", stmts, []string{"anon", "app_reader"})
	listed, _ := card["affected_statements"].([]exposureStatement)
	if len(listed) != 20 || listed[0].QueryID != 100 || listed[19].QueryID != 119 {
		t.Fatalf("affected statements = %+v, want the top 20 by calls", listed)
	}
	roles, _ := card["roles_losing_access"].([]string)
	if strings.Join(roles, ",") != "anon,app_reader" {
		t.Fatalf("roles = %v", roles)
	}
	template, _ := card["policy_template"].(string)
	if !strings.Contains(template, "CREATE POLICY") ||
		!strings.Contains(template, "ON public.leads") {
		t.Fatalf("template = %q", template)
	}
	if card["policy_template_executed"] != false {
		t.Fatal("the CREATE POLICY template is card text only, never executed")
	}
	empty := rlsApprovalCard("public", "leads", nil, nil)
	if got, _ := empty["affected_statements"].([]exposureStatement); len(got) != 0 {
		t.Fatalf("empty card statements = %v", got)
	}
}

// Verification: a statement that ran in the hour before and stops in the
// window after the revoke reverts it; missing history is not a drop.
func TestExposureReaderVerdict(t *testing.T) {
	m := func(calls int) verify.Measurement { return verify.Measurement{Samples: calls} }
	cases := []struct {
		name          string
		before, after map[int64]verify.Measurement
		regressed     bool
	}{
		{"kept", map[int64]verify.Measurement{1: m(30)}, map[int64]verify.Measurement{1: m(4)},
			false},
		{"stopped", map[int64]verify.Measurement{1: m(30), 2: m(5)},
			map[int64]verify.Measurement{1: m(9), 2: m(0)}, true},
		{"absent after", map[int64]verify.Measurement{1: m(30)}, map[int64]verify.Measurement{},
			true},
		{"idle before", map[int64]verify.Measurement{1: m(0)}, map[int64]verify.Measurement{1: m(0)},
			false},
		{"no readers", nil, nil, false},
	}
	for _, c := range cases {
		regressed, reason := judgeRevokeReaders(c.before, c.after)
		if regressed != c.regressed {
			t.Errorf("%s: regressed = %v (%s), want %v", c.name, regressed, reason, c.regressed)
		}
		if regressed && !strings.Contains(reason, "stopped") {
			t.Errorf("%s: reason %q does not say a reader stopped", c.name, reason)
		}
	}
	if w := revokeVerifyWindow(0); w != defaultRevokeVerifyWindow {
		t.Fatalf("zero window = %s, want the default %s", w, defaultRevokeVerifyWindow)
	}
	if w := revokeVerifyWindow(3 * time.Minute); w != 3*time.Minute {
		t.Fatalf("window = %s", w)
	}
}
