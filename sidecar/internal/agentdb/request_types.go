package agentdb

import "time"

// Request is an agent's ask for a database. A human (or request policy at
// creation) decides it, and an approved request is consumed by exactly one
// provision, which links it to the deployment it produced.
type Request struct {
	RequestID            string         `json:"request_id"`
	TenantID             string         `json:"tenant_id"`
	AgentID              string         `json:"agent_id"`
	OwnerID              string         `json:"owner_id"`
	RunID                string         `json:"run_id"`
	Purpose              string         `json:"purpose"`
	IsolationType        string         `json:"requested_isolation_type"`
	DatabaseName         string         `json:"database_name"`
	Provider             string         `json:"provider"`
	PolicyDecision       string         `json:"policy_decision"`
	Status               string         `json:"status"`
	IdempotencyKey       string         `json:"idempotency_key"`
	BodyHash             string         `json:"body_hash"`
	BudgetUSD            float64        `json:"budget_usd"`
	BackupRequired       bool           `json:"backup_required"`
	PolicyReasons        map[string]any `json:"policy_reasons"`
	DecidedBy            string         `json:"decided_by"`
	DecidedAt            *time.Time     `json:"decided_at"`
	ConsumedDeploymentID string         `json:"consumed_deployment_id"`
	ConsumedBy           string         `json:"consumed_by"`
	ConsumedAt           *time.Time     `json:"consumed_at"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

type RequestCreate struct {
	RequestID          string
	TenantID           string
	AgentID            string
	OwnerID            string
	RunID              string
	Purpose            string
	IsolationType      string
	DatabaseName       string
	Provider           string
	IdempotencyKey     string
	BudgetUSD          float64
	BackupRequired     bool
	DataClassification string
	MaskingPolicyID    string
	Region             string
	AllowedRegions     []string
	ApprovalSLASeconds int
	Body               map[string]any
}

// PolicyDecision is request policy's verdict at creation.
type PolicyDecision struct {
	Decision string   `json:"decision"`
	Status   string   `json:"status"`
	Reasons  []string `json:"reasons"`
}

// DecisionRequest is a human approve/deny. ActorID is the signed-in user
// and is required: an unattributed decision is not evidence.
type DecisionRequest struct {
	Decision string
	Reason   string
	ActorID  string
}

// RequestProvisionRequest provisions an approved request. TenantID,
// AgentID, Provider and ProvisioningLevel are optional expectations: when
// set they must match the request, so an approval for one agent or tenant
// cannot produce a deployment for another. ActorID is the consumer.
type RequestProvisionRequest struct {
	DeploymentID      string
	LeaseSeconds      int
	Metadata          map[string]any
	ProviderParams    map[string]any
	SizeProfileID     string
	SchemaName        string
	SecretRef         string
	SecretRefProvider string
	TenantID          string
	AgentID           string
	Provider          string
	ProvisioningLevel string
	ActorID           string
}

// BlueprintProvisionRequest plans a deployment from an approved blueprint.
// A cloud plan also consumes RequestID, an approved agent request; ActorID
// is the signed-in consumer.
type BlueprintProvisionRequest struct {
	RequestID      string
	ActorID        string
	DeploymentID   string
	TenantID       string
	AgentID        string
	RunID          string
	DatabaseName   string
	LeaseSeconds   int
	BudgetUSD      float64
	Metadata       map[string]any
	ProviderParams map[string]any
}

// TemplateProvisionRequest plans a deployment from an approved Terraform
// template, consuming RequestID the same way.
type TemplateProvisionRequest struct {
	RequestID         string
	ActorID           string
	DeploymentID      string
	TenantID          string
	AgentID           string
	RunID             string
	DatabaseName      string
	Provider          string
	ProvisioningLevel string
	LeaseSeconds      int
	BudgetUSD         float64
	Metadata          map[string]any
	ProviderParams    map[string]any
}
