package firstlook

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ident quotes a PostgreSQL identifier unless it is a plain lower-case
// name, so suggested SQL stays readable and safe to paste.
func ident(name string) string {
	bare := name != "" && (name[0] < '0' || name[0] > '9')
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			bare = false
			break
		}
	}
	if bare && !reservedWords[name] {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// reservedWords are the reserved key words most likely as object names.
var reservedWords = map[string]bool{"user": true, "order": true, "group": true,
	"table": true, "select": true, "from": true, "where": true, "default": true,
	"check": true, "column": true, "limit": true, "offset": true, "end": true,
	"primary": true, "references": true, "all": true, "analyse": true, "analyze": true}

// sizeText is a byte count for people.
func sizeText(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%d kB", b>>10)
	}
	return fmt.Sprintf("%d bytes", b)
}

// ageText is a duration in days (two days or more) or hours.
func ageText(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	h := int(d.Hours())
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}

// rowsText is an estimated row count; never-analyzed tables are unknown.
func rowsText(rows float64) string {
	if rows < 0 {
		return "unknown, never analyzed"
	}
	return fmt.Sprintf("~%.0f", rows)
}

// pct formats a share as a whole percent, with one decimal between 99%
// and 100% so that a share short of full never reads "100%".
func pct(f float64) string {
	if f >= 0.99 && f < 1 {
		return fmt.Sprintf("%.1f%%", min(math.Round(f*1000)/10, 99.9))
	}
	return fmt.Sprintf("%.0f%%", f*100)
}
