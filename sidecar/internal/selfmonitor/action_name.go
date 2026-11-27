package selfmonitor

import (
	"regexp"
	"strconv"
)

// ActionApplicationPrefix starts the application_name of an executor
// session that runs one decided action: 'pg_sage action:<decision id>'.
// It still contains ApplicationName, so ActivityExclusionSQL and
// IsApplicationName keep leaving these sessions out of every collector.
const ActionApplicationPrefix = ApplicationName + " action:"

// actionApplicationName is an exact action tag: a positive decision id of
// at most 19 digits, nothing before or after it.
var actionApplicationName = regexp.MustCompile(`^pg_sage action:([1-9][0-9]{0,18})$`)

// ActionApplicationName is the application_name of the session running
// decision decisionID's action. A decision id of 0 or below names no
// decision: the session keeps the pool's generic ApplicationName.
func ActionApplicationName(decisionID int64) string {
	if decisionID <= 0 {
		return ApplicationName
	}
	return ActionApplicationPrefix + strconv.FormatInt(decisionID, 10)
}

// ParseActionApplicationName returns the decision id an action session's
// application_name carries, and false for any other name.
func ParseActionApplicationName(name string) (int64, bool) {
	m := actionApplicationName.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil { // 19 digits beyond the int64 range
		return 0, false
	}
	return id, true
}
