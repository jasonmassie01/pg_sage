package agentdb

import (
	"context"
	"sync"
	"time"
)

type ProvisionOperation string

const (
	ProvisionOpPreflight ProvisionOperation = "preflight"
	ProvisionOpCreate    ProvisionOperation = "create"
	ProvisionOpStatus    ProvisionOperation = "status"
	ProvisionOpDestroy   ProvisionOperation = "destroy"
	ProvisionOpBackup    ProvisionOperation = "backup_check"
)

type ProvisionRequest struct {
	Operation     ProvisionOperation  `json:"operation"`
	Deployment    Deployment          `json:"deployment"`
	Plan          map[string]any      `json:"plan"`
	Policy        LiveProvisionPolicy `json:"policy"`
	RequestedAt   time.Time           `json:"requested_at"`
	DryRunCommand ProviderCommand     `json:"dry_run_command"`
}

type ProvisionResult struct {
	Status             string         `json:"status"`
	ProviderResourceID string         `json:"provider_resource_id"`
	SecretRef          string         `json:"secret_ref"`
	SecretRefProvider  string         `json:"secret_ref_provider"`
	ConnectionInfo     map[string]any `json:"connection_info"`
	Detail             map[string]any `json:"detail"`
	Error              error          `json:"-"`
}

type ProviderRunner interface {
	Name() string
	Provider() string
	Preflight(ctx context.Context, req ProvisionRequest) ProvisionResult
	Create(ctx context.Context, req ProvisionRequest) ProvisionResult
	Status(ctx context.Context, req ProvisionRequest) ProvisionResult
	Destroy(ctx context.Context, req ProvisionRequest) ProvisionResult
	BackupCheck(ctx context.Context, req ProvisionRequest) ProvisionResult
}

type RunnerRegistry struct {
	mu       sync.RWMutex
	fallback ProviderRunner
	runners  map[string]ProviderRunner
}

func NewRunnerRegistry(fallback ProviderRunner) *RunnerRegistry {
	if fallback == nil {
		fallback = DryRunProvisionRunner{}
	}
	return &RunnerRegistry{
		fallback: fallback,
		runners:  map[string]ProviderRunner{},
	}
}

func (r *RunnerRegistry) Register(runner ProviderRunner) {
	if r == nil || runner == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runners[normalizeProvider(runner.Provider())] = runner
}

func (r *RunnerRegistry) ForProvider(provider string) (ProviderRunner, error) {
	if r == nil {
		return DryRunProvisionRunner{}, nil
	}
	if !validProvider(provider) || normalizeProvider(provider) == ProviderLocalPostgres {
		return nil, ErrInvalid
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if runner, ok := r.runners[normalizeProvider(provider)]; ok {
		return runner, nil
	}
	if r.fallback == nil {
		return nil, ErrInvalid
	}
	return r.fallback, nil
}

func (r *RunnerRegistry) CommandRunnerForProvider(provider string) (ProvisionRunner, error) {
	runner, err := r.ForProvider(provider)
	if err != nil {
		return nil, err
	}
	if commandRunner, ok := runner.(ProvisionRunner); ok {
		return commandRunner, nil
	}
	return DryRunProvisionRunner{}, nil
}

func DefaultRunnerRegistry() *RunnerRegistry {
	return NewRunnerRegistry(DryRunProvisionRunner{})
}

func (DryRunProvisionRunner) Name() string { return "dry_run" }

func (DryRunProvisionRunner) Provider() string { return "*" }

func (r DryRunProvisionRunner) Preflight(
	_ context.Context,
	req ProvisionRequest,
) ProvisionResult {
	detail := map[string]any{
		"mode":           "dry_run",
		"provider":       req.Deployment.Provider,
		"operation":      req.Operation,
		"command_count":  len(req.Plan),
		"runner":         r.Name(),
		"live_supported": false,
	}
	return ProvisionResult{Status: "preflight_passed", Detail: detail}
}

func (r DryRunProvisionRunner) Create(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	result := r.Run(ctx, req.DryRunCommand)
	return commandResult("dry_run_ready", result)
}

func (r DryRunProvisionRunner) Status(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	result := r.Run(ctx, req.DryRunCommand)
	return commandResult("status_checked", result)
}

func (r DryRunProvisionRunner) Destroy(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	result := r.Run(ctx, req.DryRunCommand)
	return commandResult("destroy_dry_run_ready", result)
}

func (r DryRunProvisionRunner) BackupCheck(
	ctx context.Context,
	req ProvisionRequest,
) ProvisionResult {
	result := r.Run(ctx, req.DryRunCommand)
	return commandResult("verified", result)
}

func commandResult(status string, result ProvisionRunResult) ProvisionResult {
	if result.ExitCode != 0 {
		return ProvisionResult{Status: "failed", Detail: result.Detail, Error: ErrInvalid}
	}
	detail := RedactProviderDetail(result.Detail)
	detail["execution_mode"] = "dry_run"
	return ProvisionResult{
		Status:         status,
		ConnectionInfo: map[string]any{"execution_mode": "dry_run"},
		Detail:         detail,
	}
}

func validProvisionTransition(from, to string) bool {
	if from == to || to == "" {
		return true
	}
	transitions := map[string]map[string]bool{
		"registered": {"planned": true, "provisioned": true},
		"planned": {"preflight_passed": true, "preflight_failed": true,
			"status_checked": true, "destroy_dry_run_ready": true},
		"preflight_passed": {"provisioning": true},
		"preflight_failed": {"preflight_passed": true},
		"failed":           {"preflight_passed": true, "provisioning": true},
		"provisioning":     {"available": true, "dry_run_ready": true, "failed": true, "status_unknown": true},
		"dry_run_ready": {"preflight_passed": true, "status_checked": true,
			"provisioning": true, "destroy_pending": true},
		"available":        {"status_checked": true, "destroy_pending": true, "status_unknown": true},
		"status_checked": {"available": true, "preflight_passed": true,
			"provisioning": true, "destroy_pending": true,
			"destroy_dry_run_ready": true},
		"status_unknown":   {"status_checked": true, "available": true, "failed": true, "provisioning": true},
		"archived":         {"destroy_pending": true, "destroy_dry_run_ready": true},
		"destroy_pending":  {"destroying": true, "destroy_dry_run_ready": true},
		"destroying":       {"destroyed": true, "failed": true, "status_unknown": true},
		"queued":           {"cancel_requested": true},
		"cancel_requested": {"cancelling": true},
		"cancelling":       {"destroyed": true, "failed": true},
		"provisioned":      {"status_checked": true},
	}
	return transitions[from][to]
}

func requireProvisionTransition(from, to string) error {
	if validProvisionTransition(from, to) {
		return nil
	}
	return ErrInvalid
}
