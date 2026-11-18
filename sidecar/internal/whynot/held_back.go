package whynot

// HeldBackLink is the link a held-back decision's gate reason fails and
// the chain's sentence for it (ux-08b), so a "Held back" row and the Why
// not panel opened from it say the same thing. It is false for a reason
// that lets the change run and for one no mapping names (an older
// release's sentence): such a row keeps its own words.
func HeldBackLink(reason string) (LinkName, string, bool) {
	m, ok := mappingFor(reason)
	if !ok || m.allows {
		return "", "", false
	}
	return m.link, m.sentence, true
}
