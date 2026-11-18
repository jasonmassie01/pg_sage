package retention

// reviewExemptions: a review's read receipts age out with their run
// (review-01b); the run itself is purged by reviewRunRule.
var reviewExemptions = map[string]string{
	"review_view": "deleted with its review run (ON DELETE CASCADE); a read receipt " +
		"lives as long as the run it records",
}
