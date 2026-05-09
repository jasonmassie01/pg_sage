package agentdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound        = errors.New("agent db deployment not found")
	ErrInvalid         = errors.New("invalid agent db request")
	ErrConflict        = errors.New("agent db idempotency conflict")
	ErrRateLimited     = errors.New("agent db rate limit exceeded")
	ErrRestoreRequired = fmt.Errorf("%w: verified backup required", ErrInvalid)
)

type Store struct {
	pool *pgxpool.Pool
}

type Request struct {
	RequestID      string         `json:"request_id"`
	TenantID       string         `json:"tenant_id"`
	AgentID        string         `json:"agent_id"`
	OwnerID        string         `json:"owner_id"`
	RunID          string         `json:"run_id"`
	Purpose        string         `json:"purpose"`
	IsolationType  string         `json:"requested_isolation_type"`
	DatabaseName   string         `json:"database_name"`
	PolicyDecision string         `json:"policy_decision"`
	Status         string         `json:"status"`
	IdempotencyKey string         `json:"idempotency_key"`
	BodyHash       string         `json:"body_hash"`
	BudgetUSD      float64        `json:"budget_usd"`
	BackupRequired bool           `json:"backup_required"`
	PolicyReasons  map[string]any `json:"policy_reasons"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

const (
	ProviderLocalPostgres      = "local_postgres"
	ProviderAWSRDS             = "aws_rds"
	ProviderGCPCloudSQL        = "gcp_cloudsql"
	ProviderDatabricksLakebase = "databricks_lakebase"

	LevelSchema   = "schema"
	LevelDatabase = "database"
	LevelInstance = "instance"
)

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

type PolicyDecision struct {
	Decision string   `json:"decision"`
	Status   string   `json:"status"`
	Reasons  []string `json:"reasons"`
}

type DecisionRequest struct {
	Decision string
	Reason   string
}

type Deployment struct {
	DeploymentID       string         `json:"deployment_id"`
	TenantID           string         `json:"tenant_id"`
	AgentID            string         `json:"agent_id"`
	RunID              string         `json:"run_id"`
	DatabaseName       string         `json:"database_name"`
	Status             string         `json:"status"`
	SafetyMode         string         `json:"safety_mode"`
	IsolationType      string         `json:"isolation_type"`
	SchemaName         string         `json:"schema_name"`
	Provider           string         `json:"provider"`
	ProvisioningLevel  string         `json:"provisioning_level"`
	SizeProfileID      string         `json:"size_profile_id"`
	ProvisioningStatus string         `json:"provisioning_status"`
	BudgetUSD          float64        `json:"budget_usd"`
	BackupRequired     bool           `json:"backup_required"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	LastPingAt         *time.Time     `json:"last_ping_at,omitempty"`
	LeaseExpiresAt     *time.Time     `json:"lease_expires_at,omitempty"`
	Metadata           map[string]any `json:"metadata"`
	ProvisioningPlan   map[string]any `json:"provisioning_plan"`
	ConnectionInfo     map[string]any `json:"connection_info"`
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
	LeaseSeconds       int
	BudgetUSD          float64
	BackupRequired     bool
	Execute            bool
	Metadata           map[string]any
	ProvisioningPlan   map[string]any
	ConnectionInfo     map[string]any
}

type SizeProfile struct {
	ProfileID         string         `json:"profile_id"`
	Provider          string         `json:"provider"`
	ProvisioningLevel string         `json:"provisioning_level"`
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	CPU               float64        `json:"cpu"`
	MemoryGB          float64        `json:"memory_gb"`
	StorageGB         float64        `json:"storage_gb"`
	MaxConnections    int            `json:"max_connections"`
	MonthlyBudgetUSD  float64        `json:"monthly_budget_usd"`
	ProviderParams    map[string]any `json:"provider_params"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type ProviderCommand struct {
	Tool string   `json:"tool"`
	Args []string `json:"args"`
}

type ProvisionPlan struct {
	Provider      string            `json:"provider"`
	Level         string            `json:"provisioning_level"`
	ExecutionMode string            `json:"execution_mode"`
	Commands      []ProviderCommand `json:"commands"`
	Notes         []string          `json:"notes"`
}

type ProvisionAttempt struct {
	AttemptID    int64          `json:"attempt_id"`
	DeploymentID string         `json:"deployment_id"`
	Kind         string         `json:"kind"`
	Status       string         `json:"status"`
	Runner       string         `json:"runner"`
	Command      []string       `json:"command"`
	ExitCode     int            `json:"exit_code"`
	Stdout       string         `json:"stdout"`
	Stderr       string         `json:"stderr"`
	Detail       map[string]any `json:"detail"`
	CreatedAt    time.Time      `json:"created_at"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
}

type ProvisionRunResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Detail   map[string]any
}

type ProvisionRunner interface {
	Run(ctx context.Context, command ProviderCommand) ProvisionRunResult
}

type LifecycleBlocked struct {
	DeploymentID string `json:"deployment_id"`
	Reason       string `json:"reason"`
}

type LifecycleReconcileResult struct {
	Archived      []Deployment       `json:"archived"`
	DestroyDryRun []ProvisionAttempt `json:"destroy_dry_run"`
	Blocked       []LifecycleBlocked `json:"blocked"`
}

type ProviderReadiness struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	CLI      string `json:"cli"`
	Found    bool   `json:"found"`
	Version  string `json:"version"`
	Detail   string `json:"detail"`
}

type PingRequest struct {
	Status  string
	Metrics map[string]any
}

type LeaseRequest struct {
	LeaseSeconds int
	Reason       string
}

type Recommendation struct {
	ID                string         `json:"recommendation_id"`
	Kind              string         `json:"kind"`
	Title             string         `json:"title"`
	Detail            string         `json:"detail"`
	Status            string         `json:"status"`
	QueryFingerprint  string         `json:"query_fingerprint"`
	ActionType        string         `json:"action_type"`
	ActionRisk        string         `json:"action_risk"`
	Confidence        float64        `json:"confidence"`
	AgentInstructions map[string]any `json:"agent_instructions"`
	Payload           map[string]any `json:"payload"`
	Feedback          map[string]any `json:"feedback"`
	CreatedAt         time.Time      `json:"created_at"`
}

type RecommendationCreate struct {
	RecommendationID  string
	Kind              string
	Title             string
	Detail            string
	QueryFingerprint  string
	ActionType        string
	ActionRisk        string
	Confidence        float64
	AgentInstructions map[string]any
	Payload           map[string]any
}

type FeedbackRequest struct {
	Decision string
	Comment  string
	Applied  bool
	Result   string
	Error    string
}

type AuditEvent struct {
	AuditID      int64          `json:"audit_id"`
	DeploymentID string         `json:"deployment_id"`
	Event        string         `json:"event"`
	Detail       map[string]any `json:"detail"`
	CreatedAt    time.Time      `json:"created_at"`
}

type DeployRequest struct {
	DeployRequestID    string         `json:"deploy_request_id"`
	DeploymentID       string         `json:"deployment_id"`
	TenantID           string         `json:"tenant_id"`
	AgentID            string         `json:"agent_id"`
	RunID              string         `json:"run_id"`
	TargetDatabaseName string         `json:"target_database_name"`
	TargetSchemaName   string         `json:"target_schema_name"`
	Title              string         `json:"title"`
	Reason             string         `json:"reason"`
	Status             string         `json:"status"`
	RiskTier           string         `json:"risk_tier"`
	MigrationSQL       string         `json:"migration_sql"`
	VerificationSQL    string         `json:"verification_sql"`
	RollbackSQL        string         `json:"rollback_sql"`
	ForwardFixNotes    string         `json:"forward_fix_notes"`
	GateResults        map[string]any `json:"gate_results"`
	CreatedBy          string         `json:"created_by"`
	ReviewedBy         string         `json:"reviewed_by"`
	ReviewReason       string         `json:"review_reason"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	ReviewedAt         *time.Time     `json:"reviewed_at,omitempty"`
}

type DeployRequestCreate struct {
	DeployRequestID    string
	TargetDatabaseName string
	TargetSchemaName   string
	Title              string
	Reason             string
	Status             string
	RiskTier           string
	MigrationSQL       string
	VerificationSQL    string
	RollbackSQL        string
	ForwardFixNotes    string
	GateResults        map[string]any
	CreatedBy          string
}

type DeployRequestReview struct {
	Decision     string
	ReviewedBy   string
	ReviewReason string
}

type AgentIdentity struct {
	AgentID     string         `json:"agent_id"`
	TenantID    string         `json:"tenant_id"`
	OwnerID     string         `json:"owner_id"`
	DisplayName string         `json:"display_name"`
	Status      string         `json:"status"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

type AgentIdentityRequest struct {
	AgentID     string
	TenantID    string
	OwnerID     string
	DisplayName string
	Status      string
	Metadata    map[string]any
}

type PingToken struct {
	TokenID            string     `json:"token_id"`
	DeploymentID       string     `json:"deployment_id"`
	AgentID            string     `json:"agent_id"`
	Scope              string     `json:"scope"`
	Status             string     `json:"status"`
	Token              string     `json:"token,omitempty"`
	TokenHash          string     `json:"-"`
	FailedAttempts     int        `json:"failed_attempts"`
	RotatedFromTokenID string     `json:"rotated_from_token_id,omitempty"`
	ExpiresAt          time.Time  `json:"expires_at"`
	CreatedAt          time.Time  `json:"created_at"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
}

type PingTokenRequest struct {
	AgentID            string
	ExpiresSeconds     int
	RotatedFromTokenID string
}

type CostSampleRequest struct {
	At      time.Time
	CostUSD float64
	Metric  string
	Value   float64
	Unit    string
	Detail  map[string]any
}

type CostSummary struct {
	DeploymentID string     `json:"deployment_id"`
	TotalUSD     float64    `json:"total_usd"`
	SampleCount  int        `json:"sample_count"`
	LastSampleAt *time.Time `json:"last_sample_at,omitempty"`
	BudgetUSD    float64    `json:"budget_usd"`
	BudgetState  string     `json:"budget_state"`
	BudgetAction string     `json:"budget_action"`
}

type BudgetDecision struct {
	State  string `json:"state"`
	Action string `json:"action"`
}

type Backup struct {
	BackupID          string         `json:"backup_id"`
	DeploymentID      string         `json:"deployment_id"`
	Provider          string         `json:"provider"`
	Status            string         `json:"status"`
	ArchiveURI        string         `json:"archive_uri"`
	VerifiedAt        *time.Time     `json:"verified_at,omitempty"`
	RestoreVerifiedAt *time.Time     `json:"restore_verified_at,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	Detail            map[string]any `json:"detail"`
}

type BackupAssurance struct {
	DeploymentID   string           `json:"deployment_id"`
	Mode           string           `json:"mode"`
	BackupStatus   string           `json:"backup_status"`
	SafeForDestroy bool             `json:"safe_for_destroy"`
	Attempt        ProvisionAttempt `json:"attempt"`
	Backup         Backup           `json:"backup"`
}

type BackupRequest struct {
	BackupID          string
	Provider          string
	Status            string
	ArchiveURI        string
	VerifiedAt        time.Time
	RestoreVerifiedAt time.Time
	Detail            map[string]any
}

type TuningContext struct {
	WorkloadTypes []string
	Extensions    []string
}

type TuningHint struct {
	HintID   string         `json:"hint_id"`
	Kind     string         `json:"kind"`
	Title    string         `json:"title"`
	Detail   string         `json:"detail"`
	Severity string         `json:"severity"`
	Payload  map[string]any `json:"payload,omitempty"`
}

type CleanupDecision struct {
	Action    string `json:"action"`
	CanDelete bool   `json:"can_delete"`
	Reason    string `json:"reason"`
}
