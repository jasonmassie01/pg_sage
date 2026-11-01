package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
)

func TestDatabricksAppBootstrapAndRouterUsePlatformIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(context.Context, *pgxpool.Pool) error
	}{
		{"standalone", bootstrapAdminIfEmpty}, {"metadb", bootstrapAdminUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, ctx := firstLookDB(t)
			t.Setenv("SAGE_AUTH_MODE", auth.DatabricksAppMode)
			t.Setenv("DATABRICKS_APP_PORT", "8000")
			t.Setenv("DATABRICKS_HOST", "https://workspace.cloud.databricks.com")
			t.Setenv("SAGE_DATABRICKS_ADMIN_EMAIL", "owner@example.com")
			// Neither startup path may read or generate a local password.
			t.Setenv("SAGE_BOOTSTRAP_ADMIN_PASSWORD_FILE", "/must-not-be-read")
			if err := tc.run(ctx, p); err != nil {
				t.Fatal(err)
			}
			appAuth := &auth.DatabricksAppConfig{Workspace: "https://workspace.cloud.databricks.com", AdminEmail: "owner@example.com"}
			result := wireRouter(WireParams{Cfg: config.DefaultConfig(), Pool: p, DatabricksAppAuth: appAuth})
			for _, email := range []string{"owner@example.com", "viewer@example.com", ""} {
				r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
				if email != "" {
					r.Header.Set("X-Forwarded-Email", email)
				}
				w := httptest.NewRecorder()
				result.Handler.ServeHTTP(w, r)
				if email == "" {
					if w.Code != http.StatusUnauthorized {
						t.Fatalf("missing platform identity status=%d", w.Code)
					}
					continue
				}
				var body struct {
					ID   int    `json:"id"`
					Role string `json:"role"`
					Mode string `json:"auth_mode"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				wantRole := auth.RoleViewer
				if email == appAuth.AdminEmail {
					wantRole = auth.RoleAdmin
				}
				if w.Code != http.StatusOK || body.ID <= 0 || body.Role != wantRole || body.Mode != auth.DatabricksAppMode {
					t.Fatalf("wired App identity = %d %s", w.Code, w.Body.String())
				}
			}
			var passwords, sessions int
			if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.users WHERE password IS NOT NULL").Scan(&passwords); err != nil {
				t.Fatal(err)
			}
			if err := p.QueryRow(ctx, "SELECT count(*) FROM sage.sessions").Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if passwords != 0 || sessions != 0 {
				t.Fatalf("App created local credentials: passwords=%d sessions=%d", passwords, sessions)
			}
		})
	}
}
