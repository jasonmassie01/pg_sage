package cases

// PlanLink is a plan a case's finding is a step of (ux-03): the case shows
// "Part of plan: <goal>" linking to the plan's review in Actions.
type PlanLink struct {
	ID   int64  `json:"id"`
	Goal string `json:"goal"`
}

// AttachPlans sets c's plans.
func AttachPlans(c *Case, plans []PlanLink) {
	if c != nil {
		c.Plans, c.PlansUnavailable = plans, false
	}
}

// MarkPlansUnavailable records that c's plans could not be read, so the
// case never reads as part of no plan.
func MarkPlansUnavailable(c *Case) {
	if c != nil {
		c.Plans, c.PlansUnavailable = nil, true
	}
}
