package cloudtel

import (
	"path"
	"regexp"
	"strings"
)

// CloudSQLSocket is the instance a Cloud SQL Auth Proxy unix socket names.
type CloudSQLSocket struct {
	Project, Region, Instance string
}

var gcpRegionPattern = regexp.MustCompile(`^[a-z]+-[a-z]+[0-9]+$`)

// ParseCloudSQLSocket reads the Auth Proxy socket directory
// <dir>/<project>:<region>:<instance> (by default /cloudsql/...), which
// libpq and pgx report as the host. Anything else, including a
// domain-scoped project, is not parsed: telemetry never guesses.
func ParseCloudSQLSocket(host string) (CloudSQLSocket, bool) {
	if !strings.HasPrefix(host, "/") {
		return CloudSQLSocket{}, false
	}
	parts := strings.Split(path.Base(path.Clean(host)), ":")
	if len(parts) != 3 {
		return CloudSQLSocket{}, false
	}
	s := CloudSQLSocket{Project: parts[0], Region: parts[1], Instance: parts[2]}
	if !gcpProjectPattern.MatchString(s.Project) || !gcpRegionPattern.MatchString(s.Region) ||
		!gcpInstancePattern.MatchString(s.Instance) {
		return CloudSQLSocket{}, false
	}
	return s, true
}
