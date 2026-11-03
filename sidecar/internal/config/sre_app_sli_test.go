package config

import "testing"

func TestPushSLISourceAuthByDeploymentMode(t *testing.T) {
	t.Setenv("DATABRICKS_APP_PORT", "")
	t.Setenv("SAGE_AUTH_MODE", "")
	s := DefaultConfig().SRE.SLO
	o := SREObjectiveConfig{Name: "checkout", Database: "orders", Source: "push", Target: 0.999}
	if err := s.validateSource("fixture.", o); err == nil {
		t.Fatal("standalone accepted unsigned push")
	}
	t.Setenv("SAGE_AUTH_MODE", "databricks_app")
	if err := s.validateSource("fixture.", o); err != nil {
		t.Fatalf("platform admin ingress: %v", err)
	}
	o.Database = ""
	if err := s.validateSource("fixture.", o); err == nil {
		t.Fatal("platform accepted unbound objective")
	}
	t.Setenv("SAGE_AUTH_MODE", "")
	s.Push.HMACSecret = "0123456789abcdef0123456789abcdef"
	if err := s.validateSource("fixture.", o); err != nil {
		t.Fatalf("standalone signed ingress: %v", err)
	}
}
