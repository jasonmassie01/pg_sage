package rollout

func (set PolicySet) Resolve(instance Instance) (Policy, error) {
	if set.FleetDefault == nil {
		return Policy{}, ErrPolicyUnavailable
	}
	result := *set.FleetDefault
	result.Sources = []string{"fleet"}
	if patch, ok := set.ClassDefaults[instance.Class]; ok {
		applyPatch(&result, patch)
		result.Sources = append(result.Sources, "class:"+instance.Class)
	}
	for _, tag := range instance.Tags {
		if patch, ok := set.TagDefaults[tag]; ok {
			applyPatch(&result, patch)
			result.Sources = append(result.Sources, "tag:"+tag)
		}
	}
	if patch, ok := set.InstanceOverrides[instance.ID]; ok {
		applyPatch(&result, patch)
		result.Sources = append(result.Sources, "instance:"+instance.ID)
	}
	if err := validatePolicy(result); err != nil {
		return Policy{}, err
	}
	return result, nil
}

func applyPatch(policy *Policy, patch PolicyPatch) {
	if patch.CanaryInstances != nil {
		policy.CanaryInstances = *patch.CanaryInstances
	}
	if patch.MaxAffectedInstances != nil {
		policy.MaxAffectedInstances = *patch.MaxAffectedInstances
	}
	if patch.AggregateRegressionLimitPct != nil {
		policy.AggregateRegressionLimitPct = *patch.AggregateRegressionLimitPct
	}
	if patch.RequireLocalReverification != nil {
		policy.RequireLocalReverification = *patch.RequireLocalReverification
	}
}

func validatePolicy(policy Policy) error {
	if policy.CanaryInstances <= 0 || policy.MaxAffectedInstances <= 0 ||
		policy.CanaryInstances > policy.MaxAffectedInstances ||
		policy.AggregateRegressionLimitPct < 0 || !policy.RequireLocalReverification {
		return ErrInvalidPolicy
	}
	return nil
}
