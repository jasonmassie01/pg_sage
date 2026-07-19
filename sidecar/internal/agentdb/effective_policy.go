package agentdb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

const (
	LiveModeManual           = "manual"
	LiveModeApproval         = "approval"
	LiveModeAutoWithinPolicy = "auto_within_policy"
)

type RuntimeLiveCapability struct {
	Enabled         bool
	RunnerAvailable bool
	Policy          LiveProvisionPolicy
}

type LivePolicyLayer struct {
	Policy        LiveProvisionPolicy
	Version       int64
	ValidatedAt   time.Time
	ValidationTTL time.Duration
	Err           error
}

type EffectiveLivePolicyInput struct {
	Now           time.Time
	Runtime       *RuntimeLiveCapability
	Global        *LivePolicyLayer
	Provider      *LivePolicyLayer
	Authorization *LiveOperationAuthorization
	Operation     ProvisionOperation
	DeploymentID  string
	PlanHash      string
	Request       LiveProvisionRequest
}

type EffectiveLivePolicyResult struct {
	Policy     LiveProvisionPolicy `json:"policy"`
	Decision   LivePolicyDecision  `json:"decision"`
	PolicyHash string              `json:"policy_hash"`
}

func ResolveEffectiveLiveProvisionPolicy(
	input EffectiveLivePolicyInput,
) EffectiveLivePolicyResult {
	now := input.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	reasons := make([]string, 0, 8)
	layers := presentPolicyLayers(input, now, &reasons)
	effective := mergeLivePolicyLayers(layers, &reasons)
	validateOperationAuthorization(input, now, &reasons)
	if len(layers) == 3 && len(reasons) == 0 {
		decision := EvaluateLiveProvisionPolicy(effective, input.Request)
		reasons = append(reasons, decision.DisabledReasons...)
	}
	hash := hashEffectivePolicy(effective, input)
	decision := LivePolicyDecision{Allowed: len(reasons) == 0}
	decision.DisabledReasons = uniqueStrings(reasons)
	return EffectiveLivePolicyResult{
		Policy: effective, Decision: decision, PolicyHash: hash,
	}
}

func presentPolicyLayers(
	input EffectiveLivePolicyInput,
	now time.Time,
	reasons *[]string,
) []LiveProvisionPolicy {
	layers := make([]LiveProvisionPolicy, 0, 3)
	if !validProvider(input.Request.Provider) ||
		input.Request.Provider == ProviderLocalPostgres {
		*reasons = append(*reasons, "request provider is unknown")
	}
	if input.Runtime == nil {
		*reasons = append(*reasons, "runtime capability is missing")
	} else {
		if !input.Runtime.Enabled {
			*reasons = append(*reasons, "runtime live provisioning is disabled")
		}
		if !input.Runtime.RunnerAvailable {
			*reasons = append(*reasons, "provider runner is unavailable")
		}
		validatePolicyProvider(input.Runtime.Policy, input.Request.Provider, reasons)
		layers = append(layers, input.Runtime.Policy)
	}
	appendPolicyLayer("global", input.Global, now, &layers, reasons)
	appendPolicyLayer("provider", input.Provider, now, &layers, reasons)
	if input.Global != nil {
		validatePolicyProvider(input.Global.Policy, input.Request.Provider, reasons)
	}
	if input.Provider != nil {
		validatePolicyProvider(input.Provider.Policy, input.Request.Provider, reasons)
	}
	return layers
}

func appendPolicyLayer(
	name string,
	layer *LivePolicyLayer,
	now time.Time,
	layers *[]LiveProvisionPolicy,
	reasons *[]string,
) {
	if layer == nil {
		*reasons = append(*reasons, name+" policy is missing")
		return
	}
	if layer.Err != nil {
		*reasons = append(*reasons, name+" policy is unreadable")
	}
	if layer.ValidationTTL > 0 &&
		(layer.ValidatedAt.IsZero() || now.After(layer.ValidatedAt.Add(layer.ValidationTTL))) {
		*reasons = append(*reasons, name+" policy validation is stale")
	}
	*layers = append(*layers, layer.Policy)
}

func mergeLivePolicyLayers(
	layers []LiveProvisionPolicy,
	reasons *[]string,
) LiveProvisionPolicy {
	if len(layers) == 0 {
		return LiveProvisionPolicy{}
	}
	effective := layers[0]
	effective.ExecutionMode = normalizedLiveMode(effective.ExecutionMode)
	for i, layer := range layers {
		validateLayerPolicy(layer, reasons)
		if i > 0 {
			effective = mergeLivePolicyLayer(effective, layer)
		}
	}
	if !effective.LiveProvisioningEnabled {
		*reasons = append(*reasons, "global live provisioning is disabled")
	}
	if !effective.ProviderEnabled {
		*reasons = append(*reasons, "provider is disabled")
	}
	if effective.ExecutionMode == LiveModeManual {
		*reasons = append(*reasons, "manual mode disables live provider mutation")
	}
	return effective
}

func mergeLivePolicyLayer(
	effective LiveProvisionPolicy,
	layer LiveProvisionPolicy,
) LiveProvisionPolicy {
	effective.LiveProvisioningEnabled =
		effective.LiveProvisioningEnabled && layer.LiveProvisioningEnabled
	effective.ProviderEnabled = effective.ProviderEnabled && layer.ProviderEnabled
	effective.AllowPublicIP = effective.AllowPublicIP && layer.AllowPublicIP
	effective.RequireBackupBeforeDrop =
		effective.RequireBackupBeforeDrop || layer.RequireBackupBeforeDrop
	effective.AllowedRegions = intersectAllowlist(
		effective.AllowedRegions, layer.AllowedRegions,
	)
	effective.AllowedAccounts = intersectAllowlist(
		effective.AllowedAccounts, layer.AllowedAccounts,
	)
	effective.AllowedProjects = intersectAllowlist(
		effective.AllowedProjects, layer.AllowedProjects,
	)
	effective.AllowedWorkspaces = intersectAllowlist(
		effective.AllowedWorkspaces, layer.AllowedWorkspaces,
	)
	effective.MaxTTLSeconds = minPositiveInt(
		effective.MaxTTLSeconds, layer.MaxTTLSeconds,
	)
	effective.MaxEstimatedCostUSD = minPositiveFloat(
		effective.MaxEstimatedCostUSD, layer.MaxEstimatedCostUSD,
	)
	effective.ExecutionMode = restrictiveLiveMode(
		effective.ExecutionMode, layer.ExecutionMode,
	)
	return effective
}

func validateLayerPolicy(policy LiveProvisionPolicy, reasons *[]string) {
	if policy.MaxTTLSeconds <= 0 {
		*reasons = append(*reasons, "ttl ceiling is missing or invalid")
	}
	if policy.MaxEstimatedCostUSD <= 0 {
		*reasons = append(*reasons, "cost ceiling is missing or invalid")
	}
	if len(policy.AllowedRegions) == 0 {
		*reasons = append(*reasons, "region allowlist is empty")
	}
	if !validLiveMode(policy.ExecutionMode) {
		*reasons = append(*reasons, "execution mode is invalid")
	}
}

func validatePolicyProvider(
	policy LiveProvisionPolicy,
	provider string,
	reasons *[]string,
) {
	if policy.Provider != "" &&
		!strings.EqualFold(strings.TrimSpace(policy.Provider), strings.TrimSpace(provider)) {
		*reasons = append(*reasons, "policy provider does not match request provider")
	}
}

func validateOperationAuthorization(
	input EffectiveLivePolicyInput,
	now time.Time,
	reasons *[]string,
) {
	authz := input.Authorization
	if authz == nil {
		*reasons = append(*reasons, "operation authorization is missing")
		return
	}
	if !authz.Allowed {
		*reasons = append(*reasons, "operation authorization denied")
	}
	if authz.Provider != input.Request.Provider {
		*reasons = append(*reasons, "authorization provider does not match")
	}
	if authz.Operation != input.Operation {
		*reasons = append(*reasons, "authorization operation does not match")
	}
	if authz.DeploymentID != input.DeploymentID {
		*reasons = append(*reasons, "authorization deployment does not match")
	}
	if authz.PlanHash != input.PlanHash {
		*reasons = append(*reasons, "authorization plan does not match")
	}
	if authz.ExpiresAt.IsZero() || !authz.ExpiresAt.After(now) {
		*reasons = append(*reasons, "operation authorization is expired")
	}
}

func intersectAllowlist(left, right []string) []string {
	if len(left) == 0 || len(right) == 0 {
		return nil
	}
	leftSet, leftAll := allowlistSet(left)
	rightSet, rightAll := allowlistSet(right)
	if leftAll && rightAll {
		return []string{"*"}
	}
	if leftAll {
		return sortedSet(rightSet)
	}
	if rightAll {
		return sortedSet(leftSet)
	}
	for value := range leftSet {
		if !rightSet[value] {
			delete(leftSet, value)
		}
	}
	return sortedSet(leftSet)
}

func allowlistSet(values []string) (map[string]bool, bool) {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "*" {
			return map[string]bool{}, true
		}
		if value != "" {
			out[value] = true
		}
	}
	return out, false
}

func sortedSet(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func minPositiveInt(left, right int) int {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left < right {
		return left
	}
	return right
}

func minPositiveFloat(left, right float64) float64 {
	if left <= 0 || right <= 0 {
		return 0
	}
	if left < right {
		return left
	}
	return right
}

func restrictiveLiveMode(left, right string) string {
	left = normalizedLiveMode(left)
	right = normalizedLiveMode(right)
	if liveModeRank(left) >= liveModeRank(right) {
		return left
	}
	return right
}

func normalizedLiveMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return LiveModeApproval
	}
	return mode
}

func validLiveMode(mode string) bool {
	switch normalizedLiveMode(mode) {
	case LiveModeManual, LiveModeApproval, LiveModeAutoWithinPolicy:
		return true
	default:
		return false
	}
}

func liveModeRank(mode string) int {
	switch normalizedLiveMode(mode) {
	case LiveModeManual:
		return 3
	case LiveModeApproval:
		return 2
	case LiveModeAutoWithinPolicy:
		return 1
	default:
		return 4
	}
}

func hashEffectivePolicy(
	policy LiveProvisionPolicy,
	input EffectiveLivePolicyInput,
) string {
	payload := struct {
		Policy          LiveProvisionPolicy
		GlobalVersion   int64
		ProviderVersion int64
	}{Policy: policy}
	if input.Global != nil {
		payload.GlobalVersion = input.Global.Version
	}
	if input.Provider != nil {
		payload.ProviderVersion = input.Provider.Version
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
