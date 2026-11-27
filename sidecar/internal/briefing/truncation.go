package briefing

import "strings"

// truncationNote tells the reader the briefing ends where the model was
// stopped at its output token limit.
const truncationNote = "_(cut at the model's token limit)_"

// withTruncationNote appends truncationNote to a cut briefing. A fenced
// block left open by the cut is closed first, so the note is prose that
// grounding keeps rather than code it may replace.
func withTruncationNote(text string) string {
	if fence, open := openFence(text); open {
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += fence
	}
	return text + "\n\n" + truncationNote
}

// openFence returns the closing run for a fenced block that runs to the
// end of text, and whether there is one.
func openFence(text string) (string, bool) {
	regions := codeRegions(text)
	if len(regions) == 0 {
		return "", false
	}
	last := regions[len(regions)-1]
	if !last.fence || last.bodyEnd != len(text) || last.end != len(text) {
		return "", false
	}
	c, n, _ := fenceRun(text[last.start:last.bodyStart])
	return strings.Repeat(string(c), n), true
}
