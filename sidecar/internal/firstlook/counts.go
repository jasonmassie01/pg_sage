package firstlook

// setupRules are the catalog rules that report how pg_sage is set up (an
// extension it would like, hidden query text, an unreadable server log),
// not a problem with the database. They are never findings.
var setupRules = []string{RuleMissingExtension, RuleQueryTextHidden, RuleLogRCA}

func isSetupRule(rule string) bool {
	for _, r := range setupRules {
		if r == rule {
			return true
		}
	}
	return false
}

// IsFinding reports whether the item is a problem a DBA would act on: a
// catalog item of warning or critical severity that is not a setup note.
// Agent posture items describe pg_sage's own access, not the database, so
// they never are. Only findings set the time to first finding.
func (i Item) IsFinding() bool {
	if i.Section != "" || isSetupRule(i.Rule) {
		return false
	}
	return i.Severity == SeverityWarning || i.Severity == SeverityCritical
}

// ItemCounts sorts a report's items into kinds; every item is in exactly
// one of Findings, Setup, Posture and Notes. Critical counts the findings
// of critical severity.
type ItemCounts struct {
	Findings int
	Critical int
	// Setup counts the setup notes (setupRules) of the catalog section.
	Setup int
	// Posture counts the items outside the catalog section (the agent
	// posture section).
	Posture int
	// Notes counts the other catalog items: info severity, or a severity
	// this version does not know.
	Notes int
}

// Total is the number of items counted.
func (c ItemCounts) Total() int { return c.Findings + c.Setup + c.Posture + c.Notes }

// CountItems counts items by kind. An item of an unknown section counts
// as a posture item: it is not the catalog's.
func CountItems(items []Item) ItemCounts {
	var c ItemCounts
	for _, it := range items {
		switch {
		case it.Section != "":
			c.Posture++
		case isSetupRule(it.Rule):
			c.Setup++
		case it.IsFinding():
			c.Findings++
			if it.Severity == SeverityCritical {
				c.Critical++
			}
		default:
			c.Notes++
		}
	}
	return c
}
