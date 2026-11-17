//go:build cgo

package sqlast

import "testing"

// fleetpol-02b: the parse-tree layer accepts exactly the exposure forms of
// GRANT, REVOKE and ALTER TABLE ... ROW LEVEL SECURITY.
//
// No concurrent access tests: Check is a pure function of its input.

func TestCheckAcceptsExposureStatements(t *testing.T) {
	for _, sql := range []string{
		`REVOKE SELECT ON TABLE public.leads FROM PUBLIC`,
		`REVOKE ALL ON TABLE public.leads FROM PUBLIC`,
		`REVOKE SELECT, UPDATE ON "App"."Lead s" FROM PUBLIC`,
		`GRANT SELECT, UPDATE ON TABLE public.leads TO PUBLIC`,
		`ALTER TABLE public.leads ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE public.leads DISABLE ROW LEVEL SECURITY`,
	} {
		if err := Check(sql, Rules{}); err != nil {
			t.Errorf("Check(%q) = %v", sql, err)
		}
	}
}

func TestCheckRefusesOtherGrantForms(t *testing.T) {
	protected := func(s string) bool { return s == "pg_catalog" || s == "sage" }
	for _, sql := range []string{
		`REVOKE SELECT ON TABLE leads FROM PUBLIC`,
		`REVOKE SELECT ON TABLE public.leads FROM app_reader`,
		`REVOKE SELECT (email) ON TABLE public.leads FROM PUBLIC`,
		`REVOKE SELECT ON TABLE public.leads, public.b FROM PUBLIC`,
		`REVOKE SELECT ON TABLE public.leads FROM PUBLIC CASCADE`,
		`REVOKE GRANT OPTION FOR SELECT ON public.leads FROM PUBLIC`,
		`REVOKE SELECT ON ALL TABLES IN SCHEMA public FROM PUBLIC`,
		`GRANT SELECT ON TABLE public.leads TO PUBLIC WITH GRANT OPTION`,
		`GRANT SELECT ON TABLE public.leads TO app_reader`,
		`GRANT USAGE ON SCHEMA public TO PUBLIC`,
		`GRANT app_admin TO app_reader`,
		`REVOKE SELECT ON TABLE pg_catalog.pg_class FROM PUBLIC`,
		`ALTER TABLE public.leads FORCE ROW LEVEL SECURITY`,
		`ALTER TABLE sage.findings ENABLE ROW LEVEL SECURITY`,
	} {
		if err := Check(sql, Rules{ProtectedSchema: protected}); err == nil {
			t.Errorf("Check(%q) accepted it", sql)
		}
	}
}
