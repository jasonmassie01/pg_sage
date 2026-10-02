package srebench

import (
	"fmt"
	"strings"
)

// writeModel renders the model turn counts of the arms that had a model,
// per family: runs, turns, accepted reviews, fallbacks to the
// deterministic result (model_rejected) and disagreements with a
// conclusive graph (model_disagreed).
func (r Report) writeModel(b *strings.Builder) {
	var cells []CellRecord
	for _, c := range r.Cells {
		if c.Model != nil {
			cells = append(cells, c)
		}
	}
	if len(cells) == 0 {
		return
	}
	b.WriteString("\n## Model turn\n\n")
	b.WriteString(row("family", "arm", "runs", "model turns", "accepted reviews",
		"model_rejected", "model_disagreed"))
	b.WriteString(separator(7))
	for _, c := range cells {
		m := c.Model
		b.WriteString(row(c.Family, c.Arm, fmt.Sprint(m.Runs), fmt.Sprint(m.Turns),
			fmt.Sprint(m.Reviewed), fmt.Sprint(m.Rejected), fmt.Sprint(m.Disagreed)))
	}
}
