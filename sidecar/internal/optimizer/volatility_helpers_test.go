package optimizer

import "context"

// checkExpressionVolatility is the expression step of ValidateAndQualify
// as a verdict: the volatility tests assert only admission and reason.
func (v *Validator) checkExpressionVolatility(
	ctx context.Context, rec Recommendation,
) (bool, string) {
	_, reason := v.resolveIndexFunctions(ctx, rec)
	return reason == "", reason
}
