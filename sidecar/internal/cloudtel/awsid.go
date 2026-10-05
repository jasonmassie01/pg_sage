package cloudtel

import (
	"regexp"
	"strings"
)

// RDSHost is the RDS/Aurora resource an AWS-issued endpoint names.
type RDSHost struct {
	Identifier string // DB instance or (Cluster) DB cluster identifier
	Region     string
	Cluster    bool // an Aurora cluster endpoint
	Reader     bool // the cluster's reader endpoint (no single instance)
}

var (
	rdsIdentifierPattern = regexp.MustCompile(`^[a-z](?:[a-z0-9]|-[a-z0-9]){0,62}$`)
	awsRegionPattern     = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
	rdsLabelPattern      = regexp.MustCompile(`^(cluster-ro-|cluster-)?[a-z0-9]{6,20}$`)
)

// ValidRDSIdentifier reports an RDS instance/cluster identifier: 1-63
// letters, digits and single hyphens, starting with a letter, not ending
// with a hyphen.
func ValidRDSIdentifier(id string) bool { return rdsIdentifierPattern.MatchString(id) }

// ValidAWSRegion reports a region name such as us-east-1 or us-gov-west-1.
func ValidAWSRegion(region string) bool { return awsRegionPattern.MatchString(region) }

// ParseRDSHost names the resource of an RDS instance endpoint
// (<id>.<hash>.<region>.rds.amazonaws.com[.cn]) or an Aurora cluster
// endpoint (<id>.cluster[-ro]-<hash>...). Proxies, custom endpoints, IPs
// and custom DNS are not resolved: telemetry never discovers resources.
func ParseRDSHost(host string) (RDSHost, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	rest, ok := strings.CutSuffix(host, ".rds.amazonaws.com")
	if !ok {
		rest, ok = strings.CutSuffix(host, ".rds.amazonaws.com.cn")
	}
	if !ok {
		return RDSHost{}, false
	}
	labels := strings.Split(rest, ".")
	if len(labels) != 3 {
		return RDSHost{}, false
	}
	id, label, region := labels[0], labels[1], labels[2]
	if !ValidRDSIdentifier(id) || !ValidAWSRegion(region) ||
		!rdsLabelPattern.MatchString(label) {
		return RDSHost{}, false
	}
	h := RDSHost{Identifier: id, Region: region}
	switch {
	case strings.HasPrefix(label, "cluster-ro-"):
		h.Cluster, h.Reader = true, true
	case strings.HasPrefix(label, "cluster-"):
		h.Cluster = true
	}
	return h, true
}
