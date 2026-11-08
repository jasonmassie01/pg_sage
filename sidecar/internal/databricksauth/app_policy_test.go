package databricksauth

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	sdkconfig "github.com/databricks/databricks-sdk-go/config"
)

func TestAppSDKPinsServicePrincipalWithoutProfileFallback(t *testing.T) {
	values := map[string]string{"DATABRICKS_HOST": "workspace.cloud.databricks.com", "DATABRICKS_CLIENT_ID": "app-principal", "DATABRICKS_CLIENT_SECRET": "test-secret"}
	lookup := func(key string) string { return values[key] }
	cfg, err := appSDKConfig(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthType != "oauth-m2m" || cfg.ConfigFile != os.DevNull || cfg.Host != "https://workspace.cloud.databricks.com" || cfg.ClientID != "app-principal" || cfg.Token != "" {
		t.Fatal("App SDK is not constrained to platform service-principal OAuth")
	}
	for _, key := range []string{"DATABRICKS_TOKEN", "DATABRICKS_USERNAME", "DATABRICKS_PASSWORD", "DATABRICKS_CONFIG_PROFILE", "DATABRICKS_CONFIG_FILE", "DATABRICKS_AUTH_TYPE"} {
		t.Run(key, func(t *testing.T) {
			values[key] = "test-conflict-secret"
			defer delete(values, key)
			if _, err := appSDKConfig(lookup); err == nil || strings.Contains(err.Error(), "test-conflict-secret") {
				t.Fatal("conflicting credential source was accepted or disclosed")
			}
		})
	}
	for _, host := range []string{"", "http://workspace", "https://user:secret@workspace", "https://workspace/path", "https://workspace?secret=value", "https://workspace#fragment"} {
		values["DATABRICKS_HOST"] = host
		if _, err := appSDKConfig(lookup); err == nil {
			t.Fatalf("accepted malformed workspace origin %q", host)
		}
	}
}

func TestSafeAuthMetadataDoesNotSerializeCredentials(t *testing.T) {
	metadata := safeAuthMetadata(&sdkconfig.Config{AuthType: "oauth-m2m", Host: "https://test-user:test-password@workspace.cloud.databricks.com/path?token=test-query-secret", ClientID: "app-id", ClientSecret: "test-client-secret", Token: "test-pat-token"})
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.PrincipalID != "app-id" || metadata.AuthenticationObserved {
		t.Fatal("incorrect principal or authentication claim")
	}
	if metadata.Workspace != "https://workspace.cloud.databricks.com" {
		t.Fatal("workspace metadata was not reduced to its safe origin")
	}
	for _, field := range []string{"test-client-secret", "test-pat-token", "test-password", "test-query-secret", "authorization"} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("unsafe metadata field %s", field)
		}
	}
}
