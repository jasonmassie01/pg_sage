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
	"time"

	databricks "github.com/databricks/databricks-sdk-go"
	sdkconfig "github.com/databricks/databricks-sdk-go/config"
	"github.com/databricks/databricks-sdk-go/service/postgres"
	"github.com/databricks/databricks-sdk-go/service/sql"
	"github.com/pg-sage/sidecar/internal/appmode"
)

var workspaceOnce sync.Once
var workspaceClient *databricks.WorkspaceClient
var workspaceErr error
var workspaceMetadata atomic.Pointer[AuthMetadata]

// AuthMetadata reports configuration and the latest authentication outcomes.
// It never contains tokens, secrets, error text, or a claim that a cached token
// is still valid. AuthenticationObserved means the last successful Authorize is
// newer than the last failed one.
type AuthMetadata struct {
	Provider                  string     `json:"provider"`
	Workspace                 string     `json:"workspace,omitempty"`
	PrincipalID               string     `json:"principal_id,omitempty"`
	PrincipalKind             string     `json:"principal_kind,omitempty"`
	AuthenticationObserved    bool       `json:"authentication_observed"`
	RestartRequiredForChanges bool       `json:"restart_required_for_changes"`
	LastAuthSuccessAt         *time.Time `json:"last_auth_success_at,omitempty"`
	LastAuthFailureAt         *time.Time `json:"last_auth_failure_at,omitempty"`
	// LastFailureClass is FailureConfig, FailureOrigin or FailureAuthenticate.
	LastFailureClass string `json:"last_failure_class,omitempty"`
}

// Authorize failure classes. They name the step that failed, never the cause.
const (
	FailureConfig       = "config"
	FailureOrigin       = "origin"
	FailureAuthenticate = "authenticate"
)

// AppMode reports whether the process runs inside Databricks Apps.
func AppMode() bool {
	return appmode.InApps(os.Getenv)
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

// Postgres exposes the SDK Lakebase (Postgres) service of the unified-auth
// workspace, for the lakebase_branch clone provider (market-03).
func Postgres() (postgres.PostgresInterface, error) {
	w, err := workspace()
	if err != nil {
		return nil, err
	}
	return w.Postgres, nil
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

// workspaceAuth returns the SDK workspace host and its request authenticator;
// tests replace it with a fake.
var workspaceAuth = func() (string, func(*http.Request) error, error) {
	w, err := workspace()
	if err != nil {
		return "", nil, err
	}
	return w.Config.Host, w.Config.Authenticate, nil
}

// authNow is the clock for authentication outcomes; tests replace it.
var authNow = time.Now

// authOutcomeMu serializes outcome updates so concurrent requests never lose
// one; readers load the published metadata without it.
var authOutcomeMu sync.Mutex

// Authorize refreshes workspace authentication through the official SDK. Only
// the configured workspace origin may receive these credentials. Every call
// records its outcome for the deployment status.
func Authorize(req *http.Request) error {
	class, err := authorize(req)
	recordAuthOutcome(class)
	return err
}

// authorize reports the failure class ("" on success) with the error.
func authorize(req *http.Request) (string, error) {
	host, authenticate, err := workspaceAuth()
	if err != nil {
		return FailureConfig, fmt.Errorf("configure Databricks unified authentication: %w", err)
	}
	if err := validateOrigin(req.URL, host); err != nil {
		return FailureOrigin, err
	}
	if err := authenticate(req); err != nil {
		return FailureAuthenticate, fmt.Errorf("authenticate Databricks request: %w", err)
	}
	return "", nil
}

// recordAuthOutcome stores a success (class "") or a failure of class. The
// time is read under the lock, so recorded times only move forward.
func recordAuthOutcome(class string) {
	authOutcomeMu.Lock()
	defer authOutcomeMu.Unlock()
	metadata := CurrentAuthMetadata()
	at := authNow().UTC()
	if class == "" {
		metadata.LastAuthSuccessAt = &at
	} else {
		metadata.LastAuthFailureAt, metadata.LastFailureClass = &at, class
	}
	success, failure := metadata.LastAuthSuccessAt, metadata.LastAuthFailureAt
	metadata.AuthenticationObserved = success != nil &&
		(failure == nil || success.After(*failure))
	workspaceMetadata.Store(&metadata)
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
