package policy

import (
	"fmt"
	"strings"
	"time"
)

// WindowZone is the IANA zone a window expression names, or "" when it
// names none or does not parse.
func WindowZone(expression string) string {
	window, err := ParseWindow(expression)
	if err != nil || window.location == nil {
		return ""
	}
	return window.location.String()
}

// UnzonedWindows are the document's windows that name no zone, so they
// are evaluated on the process clock. "always" is the same on every clock.
func UnzonedWindows(doc Document) []string {
	var out []string
	for _, expression := range doc.MaintenanceWindows {
		fields := strings.Fields(expression)
		if len(fields) == 1 {
			if _, ok := alwaysKeywords[strings.ToLower(fields[0])]; ok {
				continue
			}
		}
		if _, err := ParseWindow(expression); err == nil && WindowZone(expression) == "" {
			out = append(out, expression)
		}
	}
	return out
}

var alwaysKeywords = map[string]struct{}{
	"always": {}, "anytime": {}, "24x7": {}, "24/7": {},
}

// profileDocumentIn is a profile's bootstrap document. The staffed
// profile's window is written in zone when one is given (the zone of
// trust.maintenance_window), so it is not read on the process clock.
func profileDocumentIn(profile, zone string) ([]byte, error) {
	switch profile {
	case "staffed":
		doc := StaffedProfile()
		if zone != "" {
			if _, err := time.LoadLocation(zone); err != nil {
				return nil, fmt.Errorf("staffed profile zone %q: %w", zone, err)
			}
			doc.MaintenanceWindows = []string{doc.MaintenanceWindows[0] + " " + zone}
		}
		return MarshalDocument(doc)
	case "unattended":
		return MarshalDocument(UnattendedProfile())
	default:
		return nil, ErrInvalidDocument
	}
}
