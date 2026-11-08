//go:build integration

package auth

import (
	"context"
	"errors"
	"testing"
)

func TestDatabricksAppBootstrapMigratesOnlyExplicitOwner(t *testing.T) {
	p := setupTestPool(t)
	ctx := context.Background()
	cfg := DatabricksAppConfig{Workspace: "https://workspace.cloud.databricks.com", AdminEmail: "owner@example.com"}
	id, err := CreateUser(ctx, p, cfg.AdminEmail, "previous-admin-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	session, err := CreateSession(ctx, p, id)
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := CreateUser(ctx, p, "another@example.com", "another-password", RoleViewer)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := BootstrapDatabricksAppAdmin(ctx, p, cfg); err != nil {
			t.Fatal(err)
		}
	}
	owner, err := ResolveDatabricksAppUser(ctx, p, cfg, cfg.AdminEmail)
	if err != nil || owner.ID != id || owner.Role != RoleAdmin {
		t.Fatalf("owner = %+v, %v", owner, err)
	}
	if _, err := Authenticate(ctx, p, cfg.AdminEmail, "previous-admin-password"); err == nil {
		t.Fatal("old password still authenticates")
	}
	if _, err := ValidateSession(ctx, p, session); err == nil {
		t.Fatal("old admin session survived migration")
	}
	if other, err := Authenticate(ctx, p, "another@example.com", "another-password"); err != nil || other.ID != otherID {
		t.Fatalf("unrelated account was changed: %+v, %v", other, err)
	}
	if _, err := ResolveDatabricksAppUser(ctx, p, cfg, "another@example.com"); !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("unconfigured account was silently linked: %v", err)
	}
}

func TestDatabricksAppViewerIdentityAndLiveRole(t *testing.T) {
	p := setupTestPool(t)
	ctx := context.Background()
	cfg := DatabricksAppConfig{Workspace: "https://workspace.cloud.databricks.com", AdminEmail: "owner@example.com"}
	if err := BootstrapDatabricksAppAdmin(ctx, p, cfg); err != nil {
		t.Fatal(err)
	}
	viewer, err := ResolveDatabricksAppUser(ctx, p, cfg, "viewer@example.com")
	if err != nil || viewer.ID <= 0 || viewer.Role != RoleViewer {
		t.Fatalf("viewer = %+v, %v", viewer, err)
	}
	if _, err := p.Exec(ctx, "UPDATE sage.users SET role='operator' WHERE id=$1", viewer.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := ResolveDatabricksAppUser(ctx, p, cfg, viewer.Email)
	if err != nil || updated.ID != viewer.ID || updated.Role != RoleOperator {
		t.Fatalf("stale role or identity: %+v, %v", updated, err)
	}
	otherWorkspace := cfg
	otherWorkspace.Workspace = "https://another.cloud.databricks.com"
	if _, err := ResolveDatabricksAppUser(ctx, p, otherWorkspace, viewer.Email); !errors.Is(err, ErrOAuthLinkRequired) {
		t.Fatalf("identity crossed workspace boundary: %v", err)
	}
}

func TestDatabricksAppBootstrapRefusesOtherIdentityAtomically(t *testing.T) {
	p := setupTestPool(t)
	ctx := context.Background()
	cfg := DatabricksAppConfig{Workspace: "https://workspace.cloud.databricks.com", AdminEmail: "owner@example.com"}
	id, err := CreateUser(ctx, p, cfg.AdminEmail, "previous-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, "UPDATE sage.users SET oauth_issuer='https://other.example.com', oauth_subject='other' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	session, err := CreateSession(ctx, p, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := BootstrapDatabricksAppAdmin(ctx, p, cfg); err == nil {
		t.Fatal("conflicting external identity overwritten")
	}
	if _, err := Authenticate(ctx, p, cfg.AdminEmail, "previous-password"); err != nil {
		t.Fatal("refusal mutated password")
	}
	if _, err := ValidateSession(ctx, p, session); err != nil {
		t.Fatal("refusal revoked session")
	}
}
