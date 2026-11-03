// Package databricksauth integrates Databricks unified authentication without
// persisting short-lived workspace or database tokens in pg_sage configuration.
package databricksauth

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	databricks "github.com/databricks/databricks-sdk-go"
	sdkconfig "github.com/databricks/databricks-sdk-go/config"
	"github.com/databricks/databricks-sdk-go/service/sql"
)

var workspaceOnce sync.Once
var workspaceClient *databricks.WorkspaceClient
var workspaceErr error
var workspaceMetadata atomic.Pointer[AuthMetadata]

// AuthMetadata reports configuration and a past successful authentication only.
// It never contains tokens, secrets, or a claim that a cached token is still valid.
type AuthMetadata struct {
	Provider                  string `json:"provider"`
	Workspace                 string `json:"workspace,omitempty"`
	PrincipalID               string `json:"principal_id,omitempty"`
	PrincipalKind             string `json:"principal_kind,omitempty"`
	AuthenticationObserved    bool   `json:"authentication_observed"`
	RestartRequiredForChanges bool   `json:"restart_required_for_changes"`
}

func AppMode() bool {
	return os.Getenv("SAGE_AUTH_MODE") == "databricks_app" || os.Getenv("DATABRICKS_APP_PORT") != ""
}

func appSDKConfig(getenv func(string) string) (*sdkconfig.Config, error) {
	for _, name := range []string{"DATABRICKS_TOKEN", "DATABRICKS_USERNAME", "DATABRICKS_PASSWORD", "DATABRICKS_CONFIG_PROFILE", "DATABRICKS_CONFIG_FILE"} {
		if getenv(name) != "" {
			return nil, fmt.Errorf("app OAuth requires removing %s", name)
		}
	}
	if method := getenv("DATABRICKS_AUTH_TYPE"); method != "" && method != "oauth-m2m" {
		return nil, fmt.Errorf("app mode requires DATABRICKS_AUTH_TYPE=oauth-m2m")
	}
	host := strings.TrimRight(getenv("DATABRICKS_HOST"), "/")
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("app mode requires an HTTPS DATABRICKS_HOST workspace origin")
	}
	if getenv("DATABRICKS_CLIENT_ID") == "" || getenv("DATABRICKS_CLIENT_SECRET") == "" {
		return nil, fmt.Errorf("app mode requires platform service-principal OAuth credentials")
	}
	return &sdkconfig.Config{Host: host, ClientID: getenv("DATABRICKS_CLIENT_ID"), ClientSecret: getenv("DATABRICKS_CLIENT_SECRET"), AuthType: "oauth-m2m", ConfigFile: os.DevNull}, nil
}

func CurrentAuthMetadata() AuthMetadata {
	if metadata := workspaceMetadata.Load(); metadata != nil {
		return *metadata
	}
	provider := "uninitialized"
	if AppMode() {
		provider = "oauth-m2m"
	}
	return AuthMetadata{Provider: provider, RestartRequiredForChanges: true}
}

func safeAuthMetadata(cfg *sdkconfig.Config) *AuthMetadata {
	metadata := &AuthMetadata{Provider: cfg.AuthType, RestartRequiredForChanges: true}
	if u, err := url.Parse(cfg.Host); err == nil && u.Scheme != "" && u.Host != "" {
		metadata.Workspace = u.Scheme + "://" + u.Host
	}
	if metadata.Provider == "" {
		metadata.Provider = "unified-default"
	}
	if cfg.AuthType == "oauth-m2m" {
		metadata.PrincipalKind = "service_principal"
		metadata.PrincipalID = cfg.ClientID
	}
	return metadata
}

// StatementExecution exposes only the SDK SQL API, never raw workspace credentials.
func StatementExecution() (sql.StatementExecutionInterface, error) {
	w, err := workspace()
	if err != nil {
		return nil, err
	}
	return w.StatementExecution, nil
}

// WorkspaceOrigin binds external resource scopes to the actual SDK workspace.
func WorkspaceOrigin() (string, error) {
	w, err := workspace()
	if err != nil {
		return "", err
	}
	u, err := url.Parse(w.Config.Host)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("configured SDK workspace is not an HTTPS origin")
	}
	return "https://" + u.Host, nil
}

func workspace() (*databricks.WorkspaceClient, error) {
	workspaceOnce.Do(func() {
		if AppMode() {
			cfg, err := appSDKConfig(os.Getenv)
			if err != nil {
				workspaceErr = err
				return
			}
			workspaceClient, workspaceErr = databricks.NewWorkspaceClient((*databricks.Config)(cfg))
		} else {
			workspaceClient, workspaceErr = databricks.NewWorkspaceClient()
		}
		if workspaceErr == nil {
			workspaceMetadata.Store(safeAuthMetadata(workspaceClient.Config))
		}
	})
	return workspaceClient, workspaceErr
}

// Authorize refreshes workspace authentication through the official SDK. Only
// the configured workspace origin may receive these credentials.
func Authorize(req *http.Request) error {
	w, err := workspace()
	if err != nil {
		return fmt.Errorf("configure Databricks unified authentication: %w", err)
	}
	if err := validateOrigin(req.URL, w.Config.Host); err != nil {
		return err
	}
	if err := w.Config.Authenticate(req); err != nil {
		return fmt.Errorf("authenticate Databricks request: %w", err)
	}
	metadata := CurrentAuthMetadata()
	metadata.AuthenticationObserved = true
	workspaceMetadata.Store(&metadata)
	return nil
}

func validateOrigin(target *url.URL, workspaceHost string) error {
	origin, err := url.Parse(workspaceHost)
	if err != nil || origin.Scheme != "https" || origin.Host == "" ||
		target == nil || target.Scheme != "https" || target.User != nil ||
		!strings.EqualFold(target.Host, origin.Host) {
		return fmt.Errorf("databricks LLM endpoint must use the configured HTTPS workspace origin")
	}
	return nil
}
