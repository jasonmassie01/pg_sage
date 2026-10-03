//go:build race

package autonomy

// raceDetector: the race detector slows Go code 2-20x, so wall-clock
// budgets of tests that do real Go work are scaled (see floodBudget).
const raceDetector = true
