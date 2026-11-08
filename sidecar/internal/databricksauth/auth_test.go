package databricksauth

import (
	"net/url"
	"testing"
)

func TestWorkspaceCredentialsStayOnConfiguredHTTPSOrigin(t *testing.T) {
	for _, tc := range []struct {
		target string
		ok     bool
	}{
		{"https://workspace.cloud.databricks.com/ai-gateway/mlflow/v1/chat/completions", true},
		{"https://WORKSPACE.cloud.databricks.com/ai-gateway/mlflow/v1", true},
		{"http://workspace.cloud.databricks.com/ai-gateway/mlflow/v1", false},
		{"https://other.cloud.databricks.com/ai-gateway/mlflow/v1", false},
		{"https://workspace.cloud.databricks.com.attacker.example/ai-gateway/mlflow/v1", false},
		{"https://workspace.cloud.databricks.com:444/ai-gateway/mlflow/v1", false},
		{"https://user@workspace.cloud.databricks.com/ai-gateway/mlflow/v1", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateOrigin(u, "https://workspace.cloud.databricks.com"); (err == nil) != tc.ok {
				t.Fatalf("origin validation = %v; want allowed=%v", err, tc.ok)
			}
		})
	}
}
