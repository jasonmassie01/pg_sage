//go:build !cgo

package sqlast

// ParsePlannableAlter needs the libpg_query parser; without cgo it
// reports ErrUnavailable and the planner refuses to plan.
func ParsePlannableAlter(string) (PlannableAlter, error) {
	return PlannableAlter{}, ErrUnavailable
}
