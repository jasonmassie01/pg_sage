package agentdb

import "time"

type Deployment struct {
	DeploymentID              string         `json:"deployment_id"`
	TenantID                  string         `json:"tenant_id"`
	AgentID                   string         `json:"agent_id"`
	RunID                     string         `json:"run_id"`
	DatabaseName              string         `json:"database_name"`
	Status                    string         `json:"status"`
	SafetyMode                string         `json:"safety_mode"`
	IsolationType             string         `json:"isolation_type"`
	SchemaName                string         `json:"schema_name"`
	Provider                  string         `json:"provider"`
	ProvisioningLevel         string         `json:"provisioning_level"`
	SizeProfileID             string         `json:"size_profile_id"`
	ProvisioningStatus        string         `json:"provisioning_status"`
	ProviderResourceID        string         `json:"provider_resource_id"`
	SecretRef                 string         `json:"secret_ref"`
	SecretRefProvider         string         `json:"secret_ref_provider"`
	SecretRefExpiresAt        *time.Time     `json:"secret_ref_expires_at,omitempty"`
	LiveMode                  bool           `json:"live_mode"`
	BudgetUSD                 float64        `json:"budget_usd"`
	BackupRequired            bool           `json:"backup_required"`
	CreatedAt                 time.Time      `json:"created_at"`
	UpdatedAt                 time.Time      `json:"updated_at"`
	LastPingAt                *time.Time     `json:"last_ping_at,omitempty"`
	LeaseExpiresAt            *time.Time     `json:"lease_expires_at,omitempty"`
	Metadata                  map[string]any `json:"metadata"`
	ProvisioningPlan          map[string]any `json:"provisioning_plan"`
	ConnectionInfo            map[string]any `json:"connection_info"`
	LifecycleVersion          int64          `json:"lifecycle_version"`
	CleanupClaimID            string         `json:"cleanup_claim_id,omitempty"`
	CleanupClaimedAt          *time.Time     `json:"cleanup_claimed_at,omitempty"`
	TeardownOperationID       string         `json:"teardown_operation_id,omitempty"`
	ProviderMutationID        string         `json:"provider_mutation_id,omitempty"`
	ProviderMutationExpiresAt *time.Time     `json:"provider_mutation_expires_at,omitempty"`
	// AgentStatus is the agent-reported heartbeat health; it never drives lifecycle.
	AgentStatus string `json:"agent_status"`
	// TeardownBlockedReason explains why the TTL reconciler cannot destroy a
	// live resource yet; it is cleared once teardown is authorized.
	TeardownBlockedReason string     `json:"teardown_blocked_reason,omitempty"`
	TeardownBlockedAt     *time.Time `json:"teardown_blocked_at,omitempty"`
	// CreateOperationID is recorded before a live create is sent to a provider.
	CreateOperationID string `json:"create_operation_id,omitempty"`
}

type RegisterRequest struct {
	DeploymentID       string
	TenantID           string
	AgentID            string
	RunID              string
	DatabaseName       string
	SafetyMode         string
	IsolationType      string
	SchemaName         string
	Provider           string
	ProvisioningLevel  string
	SizeProfileID      string
	ProvisioningStatus string
	ProviderResourceID string
	SecretRef          string
	SecretRefProvider  string
	SecretRefExpiresAt *time.Time
	LiveMode           bool
	LeaseSeconds       int
	BudgetUSD          float64
	BackupRequired     bool
	Execute            bool
	Metadata           map[string]any
	ProvisioningPlan   map[string]any
	ConnectionInfo     map[string]any
}
