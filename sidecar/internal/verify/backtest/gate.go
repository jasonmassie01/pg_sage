package backtest

import (
	"fmt"
	"math"
	"strconv"

	"github.com/pg-sage/sidecar/internal/verify"
)

// The gate lab-01c's auto reads (spec 5.2, G-T5): v2 acts on a database
// whose latest back-test is of v2, has at least GateMinPlacebos usable
// placebos, a false-positive rate at or below GateMaxFPRate and a
// detection rate at or above GateMinDetection. The detection floor keeps
// a verifier that never decides (rate 0) from passing vacuously. The
// Wilson bound is shown, not gated, so a small database is not held back
// by its size alone. The age bar (35 days) is verify.BacktestMaxAge.
const (
	GateMinPlacebos  = 200
	GateMaxFPRate    = 0.05
	GateMinDetection = 0.80
)

// Gate reports whether the back-test passes and, when not, the first
// reason. The rates compare as exact fractions of the counts, so a
// boundary (exactly 5%, exactly 80%) is never lost to rounding.
func Gate(verifier string, c Counts) (bool, string) {
	switch {
	case verifier != verify.VerifierV2:
		return false, fmt.Sprintf("the back-test is of verifier %q, not %q", verifier,
			verify.VerifierV2)
	case c.Usable() < GateMinPlacebos:
		return false, fmt.Sprintf("%d usable placebos below %d", c.Usable(),
			GateMinPlacebos)
	case c.FalsePositives*20 > c.Usable():
		return false, fmt.Sprintf("false-positive rate %s above %s", Pct(c.FPRate()),
			Pct(GateMaxFPRate))
	case c.Injected <= 0 || c.Detected*5 < c.Injected*4:
		return false, fmt.Sprintf("detection %s below %s", Pct(c.DetectionRate()),
			Pct(GateMinDetection))
	}
	return true, ""
}

// Gate is r's verifier and v2 counts through Gate.
func (r Result) Gate() (bool, string) { return Gate(r.Verifier, r.V2) }

// Backtest is r as lab-01c's resolver reads it.
func (r Result) Backtest() verify.Backtest {
	passed, _ := r.Gate()
	return verify.Backtest{Passed: passed, RanAt: r.RanAt}
}

// Pct is a share as a percentage with at most two decimals: 0.05 is
// "5%", 0.0501 "5.01%", 0 "0%".
func Pct(share float64) string {
	return strconv.FormatFloat(math.Round(share*10000)/100, 'f', -1, 64) + "%"
}
