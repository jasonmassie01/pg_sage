package budget

import (
	"fmt"
	"time"
)

// The revert rule of an enforced statement_timeout (market-08b).
const (
	// RevertFloor keeps a zero or tiny prediction from reverting on a
	// single cancel: the threshold is never below 2 x RevertFloor.
	RevertFloor = 5
	// RevertMinElapsed is how long after apply the rule first applies.
	RevertMinElapsed = 15 * time.Minute
	// RevertWatch is how long after apply the rule applies.
	RevertWatch = 24 * time.Hour
	// predictionDays is the promotion days the prediction averages.
	predictionDays = 7
)

// NoLogApprovalReason is the approval card text of an enforce finding
// whose revert cannot be verified: without the server log nothing counts
// the cancels it causes.
const NoLogApprovalReason = "auto-revert unavailable: no statement log"

// RevertCheck is one evaluation of an enforced timeout: the cancels
// counted since apply, the predicted cancels per hour and the time since
// apply.
type RevertCheck struct {
	Actual           int64
	PredictedPerHour float64
	Elapsed          time.Duration
	// LowerBound marks a count the log subscription may have cut short.
	LowerBound bool
}

// Predicted is the cancels the prediction expects over the elapsed time.
func (r RevertCheck) Predicted() float64 {
	return max(r.PredictedPerHour, 0) * r.Elapsed.Hours()
}

// Threshold is the count past which the timeout reverts: twice the
// prediction, never below twice RevertFloor.
func (r RevertCheck) Threshold() float64 {
	return 2 * max(r.Predicted(), RevertFloor)
}

// Decide applies the rule: revert when the cancels since apply exceed
// 2 x max(predicted, 5), at least 15 minutes and at most 24 hours after
// apply. reason states the numbers either way.
func (r RevertCheck) Decide() (revert bool, reason string) {
	numbers := fmt.Sprintf("%d cancels in %.1f h, prediction %.1f (%.1f/h), threshold %.0f",
		r.Actual, r.Elapsed.Hours(), r.Predicted(), max(r.PredictedPerHour, 0), r.Threshold())
	if r.LowerBound {
		numbers += " (a lower bound: the log subscription dropped entries)"
	}
	switch {
	case r.Elapsed < RevertMinElapsed:
		return false, numbers + ": too early to judge (15 minutes)"
	case r.Elapsed > RevertWatch:
		return false, numbers + ": the 24 h watch ended"
	case float64(r.Actual) > r.Threshold():
		return true, numbers + ": more than twice the prediction; reverting"
	}
	return false, numbers + ": within twice the prediction"
}

// PredictedPerHour is the budget's predicted cancels per hour once
// enforced: its mean daily would-cancels plus actual cancels over the
// last 7 full UTC days before now's day, over 24. days is how many rows
// it averaged (0: no prediction, 0/h).
func PredictedPerHour(history []ShadowDay, now time.Time) (perHour float64, days int) {
	today := utcDay(now)
	var sum int64
	for i := len(history) - 1; i >= 0 && days < predictionDays; i-- {
		d := history[i]
		if !utcDay(d.Day).Before(today) {
			continue
		}
		sum += int64(d.WouldCancel) + d.ActualCancels
		days++
	}
	if days == 0 {
		return 0, 0
	}
	return float64(sum) / float64(days) / 24, days
}

// CanPromoteSince is CanPromote counting only the shadow data after the
// budget's last revert (zero: none): re-promotion needs a fresh 7 days
// with no revert in them.
func CanPromoteSince(history []ShadowDay, now, revertedAt time.Time) (bool, string) {
	if revertedAt.IsZero() {
		return CanPromote(history, now)
	}
	var since []ShadowDay
	for _, d := range history {
		if utcDay(d.Day).Before(utcDay(revertedAt)) {
			continue
		}
		if d.FirstSeen.Before(revertedAt) {
			d.FirstSeen = revertedAt
		}
		since = append(since, d)
	}
	if span := now.Sub(revertedAt); span < PromotionWindow {
		return false, fmt.Sprintf("its enforced timeout was reverted %s ago (%s); "+
			"re-promotion needs 7 days of warn-mode data since", spanText(span),
			revertedAt.UTC().Format(time.RFC3339))
	}
	return CanPromote(since, now)
}
