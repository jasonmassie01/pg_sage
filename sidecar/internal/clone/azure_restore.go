package clone

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/pg-sage/sidecar/internal/egress"
)

// azure_restore (lab-05g): Azure Database for PostgreSQL flexible server
// has no copy-on-write clone, so a clone is a point-in-time restore of
// the monitored server to a new server in the same region and major
// version: a full copy at the full server price, made only with
// clone.allow_full_copy. AzureRestoreAPI is the SnapshotAPI of it;
// SnapshotProvider wraps it as a Provider.

// Azure restore errors. Each is distinguishable with errors.Is.
var (
	// ErrInvalidAzureRestoreConfig: a subscription, resource group,
	// server, deadline, TTL, clone name or DSN outside the accepted shape,
	// or a server pg_sage did not make.
	ErrInvalidAzureRestoreConfig = errors.New("clone: invalid azure_restore configuration")
	// ErrNotAzureFlexibleServer: the configured server is not a primary
	// flexible server the monitored DSN reaches on its FQDN.
	ErrNotAzureFlexibleServer = errors.New("clone: the monitored database is not the " +
		"configured Azure flexible server")
	// ErrAzureNotFound: ARM describes no such resource.
	ErrAzureNotFound = errors.New("clone: azure resource not found")
	// ErrAzureAlreadyExists: a server of that name exists, here or (names
	// are global DNS names) in another subscription.
	ErrAzureAlreadyExists = errors.New("clone: azure server name already in use")
	// ErrAzurePermission: ARM refused the call (401 or 403); the error
	// names the RBAC action the call needs.
	ErrAzurePermission = errors.New("clone: azure permission denied")
)

const (
	// AzureReceiptTag and AzureExpiresTag mark a restored server: the
	// receipt's name, so the reaper finds a leaked one, and its expiry in
	// Unix seconds. Azure has no server-side expiry.
	AzureReceiptTag = "pg-sage-receipt"
	AzureExpiresTag = "pg-sage-expires-at"
	// AzureARMBase is Azure Resource Manager (public cloud).
	AzureARMBase = "https://management.azure.com"
	// AzureMaxTTL bounds a clone's life: a leaked one holds production
	// data for at most a day.
	AzureMaxTTL = 24 * time.Hour
	// AzureRestoreLag is how far before now the restore's point in time
	// is: the source's write-ahead log has reached backup storage by then.
	AzureRestoreLag = 5 * time.Minute
	// AzureCloneBackupDays is a clone's backup retention, Azure's minimum.
	AzureCloneBackupDays = 7
	defaultAzurePoll     = 15 * time.Second
	// azureCallTimeout bounds one lookup or destroy.
	azureCallTimeout = 5 * time.Minute
)

var (
	azureSubscription = regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	azureGroup      = regexp.MustCompile(`^[A-Za-z0-9._()-]{0,89}[A-Za-z0-9_()-]$`)
	azureServerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
)

// AzureProduction is the monitored (production) database a clone is
// restored from: its DSN names the source server's FQDN, and the clone's
// DSN reuses it (same users, restored data).
type AzureProduction struct{ DSN string }

// AzureRestoreConfig configures an AzureRestoreAPI.
type AzureRestoreConfig struct {
	// SubscriptionID, ResourceGroup and Server name the monitored flexible
	// server; clones are restored in the same resource group and region.
	SubscriptionID, ResourceGroup, Server string
	// Deadline bounds one restore, from the request until the server is
	// ready (clone.restore_deadline_minutes). One not ready by then is
	// left to the reaper (an uncertain create).
	Deadline time.Duration
	// TTL is the longest a clone may live: its expiry tag and MaxTTL. It
	// must exceed Deadline.
	TTL time.Duration
	// Token is an Azure Resource Manager bearer token (the chain
	// azure.DefaultTokenSource builds); nil, an error or an empty token
	// makes the provider unavailable.
	Token func(context.Context) (string, error)
	// Production reads production's DSN at each restore; nil makes every
	// restore unavailable (an adapter that only destroys, as the reaper's).
	Production func(context.Context) (AzureProduction, error)
	BaseURL    string        // "": AzureARMBase
	HTTPClient *http.Client  // nil: the egress floor's client
	PollEvery  time.Duration // operation poll period; 0: 15 seconds
	Now        func() time.Time
}

// AzureRestoreAPI restores the monitored flexible server to a new server
// tagged with its receipt name.
type AzureRestoreAPI struct{ cfg AzureRestoreConfig }

// NewAzureRestoreAPI validates cfg; it contacts nothing.
func NewAzureRestoreAPI(cfg AzureRestoreConfig) (*AzureRestoreAPI, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	cfg.SubscriptionID = strings.ToLower(cfg.SubscriptionID)
	if cfg.BaseURL == "" {
		cfg.BaseURL = AzureARMBase
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = egress.HTTPClient(0)
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultAzurePoll
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &AzureRestoreAPI{cfg: cfg}, nil
}

func (cfg AzureRestoreConfig) validate() error {
	switch {
	case !azureSubscription.MatchString(cfg.SubscriptionID):
		return fmt.Errorf("%w: subscription %q must be an Azure subscription ID (a GUID)",
			ErrInvalidAzureRestoreConfig, cfg.SubscriptionID)
	case !azureGroup.MatchString(cfg.ResourceGroup) || strings.Contains(cfg.ResourceGroup, ".."):
		return fmt.Errorf("%w: resource group %q must be 1-90 letters, digits, '.', '_', "+
			"'-' or parentheses, not ending in '.'", ErrInvalidAzureRestoreConfig,
			cfg.ResourceGroup)
	case !azureServerName.MatchString(cfg.Server) || auroraOwnedName.MatchString(cfg.Server):
		return fmt.Errorf("%w: server %q must be a flexible server name (3-63 lowercase "+
			"letters, digits and hyphens) that is not a pg_sage clone",
			ErrInvalidAzureRestoreConfig, cfg.Server)
	case cfg.Deadline < RDSRestoreMinDeadline || cfg.Deadline > RDSRestoreMaxDeadline:
		return fmt.Errorf("%w: restore deadline %s must be within %s-%s",
			ErrInvalidAzureRestoreConfig, cfg.Deadline, RDSRestoreMinDeadline,
			RDSRestoreMaxDeadline)
	case cfg.TTL <= cfg.Deadline || cfg.TTL > AzureMaxTTL:
		return fmt.Errorf("%w: clone TTL %s must exceed the restore deadline %s and be at "+
			"most %s", ErrInvalidAzureRestoreConfig, cfg.TTL, cfg.Deadline, AzureMaxTTL)
	}
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%w: ARM base URL must be an http(s) URL",
				ErrInvalidAzureRestoreConfig)
		}
	}
	return nil
}

// NewAzureRestoreProvider is the azure_restore Provider: SnapshotProvider
// over an AzureRestoreAPI of cfg.
func NewAzureRestoreProvider(cfg AzureRestoreConfig) (*SnapshotProvider, error) {
	api, err := NewAzureRestoreAPI(cfg)
	if err != nil {
		return nil, err
	}
	return NewSnapshotProvider(api, api.cfg.Now)
}

// client is ARM with a usable token, or ErrCloneUnavailable.
func (a *AzureRestoreAPI) client(ctx context.Context) (*azureHTTP, error) {
	const hint = "; rehearsals are recommend-only until then (set AZURE_TENANT_ID, " +
		"AZURE_CLIENT_ID and AZURE_CLIENT_SECRET, use a managed identity, or run az login)"
	if a.cfg.Token == nil {
		return nil, fmt.Errorf("%w: %s has no Azure credentials%s", ErrCloneUnavailable,
			AzureRestoreAdapter, hint)
	}
	tok, err := a.cfg.Token(ctx)
	if err == nil && blank(tok) {
		err = errors.New("the credential chain returned an empty token")
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %s has no usable Azure credentials: %s%s",
			ErrCloneUnavailable, AzureRestoreAdapter, sanitizeARM(err.Error()), hint)
	}
	return &azureHTTP{base: a.cfg.BaseURL, sub: a.cfg.SubscriptionID,
		group: a.cfg.ResourceGroup, token: a.cfg.Token, client: a.cfg.HTTPClient,
		poll: a.cfg.PollEvery}, nil
}

// Describe: the clone is ready in about SnapshotTypicalReady (it scales
// with the data) and lives at most TTL. SnapshotProvider makes it a full
// copy.
func (a *AzureRestoreAPI) Describe(context.Context) (Capability, error) {
	return Capability{Kind: KindFullCopy, TypicalReady: SnapshotTypicalReady,
		MaxTTL: a.cfg.TTL}, nil
}

// TargetScope is the receipt scope of this provider's clones: the reaper
// looks in the same subscription and resource group.
func (a *AzureRestoreAPI) TargetScope() string {
	return "azure/" + a.cfg.SubscriptionID + "/" + a.cfg.ResourceGroup
}

// checkAzureName refuses a name that is not a pg_sage clone name
// (sage-<purpose>-<8 hex>) or that a flexible server cannot take.
func checkAzureName(name, what string) error {
	if !auroraOwnedName.MatchString(name) || !azureServerName.MatchString(name) {
		return fmt.Errorf("%w: %q is not a pg_sage clone name (sage-<purpose>-<8 hex>, "+
			"lowercase, 3-63 characters); %s", ErrInvalidAzureRestoreConfig, name, what)
	}
	return nil
}

// FindByName is the server named name tagged with that name. found is
// false only when ARM describes none. A server of that name not tagged
// for this receipt is an error, never "not found": the restore tags the
// server as it creates it, so pg_sage cannot prove it is its own and must
// not delete it, but closing the receipt as gone would lose track of a
// billable copy of production. The reaper leaves the receipt unresolved.
func (a *AzureRestoreAPI) FindByName(ctx context.Context, name string) (string, bool,
	error) {
	api, err := a.client(ctx)
	if err != nil {
		return "", false, err
	}
	if err := checkAzureName(name, "not looked up"); err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(ctx, azureCallTimeout)
	defer cancel()
	s, err := api.GetServer(ctx, name)
	if errors.Is(err, ErrAzureNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("azure_restore: find server %s: %w", name, err)
	}
	if tag := s.Tags[AzureReceiptTag]; tag != name {
		return "", false, fmt.Errorf("%w: azure_restore: server %s exists but its tag %s "+
			"is %q, not its own name, so pg_sage cannot prove it is its own and leaves it "+
			"and its receipt; %s", ErrCloneLeftBehind, name, AzureReceiptTag, tag,
			a.byHand(name))
	}
	return name, true, nil
}

// byHand is how an operator deletes a server pg_sage will not.
func (a *AzureRestoreAPI) byHand(name string) string {
	return fmt.Sprintf("delete it by hand if it is a clone (az postgres flexible-server "+
		"delete --subscription %s --resource-group %s --name %s --yes)",
		a.cfg.SubscriptionID, a.cfg.ResourceGroup, name)
}

// Destroy deletes a server this provider made. The server is read first:
// one not named sage-<purpose>-<8 hex> or not tagged for this receipt is
// refused and nothing is deleted. One gone or already dropping counts as
// deleted.
func (a *AzureRestoreAPI) Destroy(ctx context.Context, id string) error {
	api, err := a.client(ctx)
	if err != nil {
		return err
	}
	if err := checkAzureName(id, "nothing deleted"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, azureCallTimeout)
	defer cancel()
	return api.deleteOwned(ctx, id)
}

var (
	_ SnapshotAPI = (*AzureRestoreAPI)(nil)
	_ Describer   = (*AzureRestoreAPI)(nil)
	_ Finder      = (*AzureRestoreAPI)(nil)
)
