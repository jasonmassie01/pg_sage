package analyzer

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ruleReplicationLag flags replicas with high replay lag.
// Warning if > 30s, critical if > 5 minutes. A replica is named by its
// application_name and client address, not its row position: the order
// of pg_stat_replication rows is not stable across cycles. Replicas that
// share a name merge into one finding at the worst lag.
func ruleReplicationLag(
	current *collector.Snapshot,
	_ *collector.Snapshot,
	_ *config.Config,
	_ *RuleExtras,
) []Finding {
	if current.Replication == nil {
		return nil
	}
	worst := map[string]*replicaLag{}
	var keys []string
	for _, r := range current.Replication.Replicas {
		if r.ReplayLag == nil || *r.ReplayLag == "" {
			continue
		}
		key := replicaIdentity(r)
		lag := parsePGInterval(*r.ReplayLag)
		w, seen := worst[key]
		if !seen {
			w = &replicaLag{replica: r, lag: lag}
			worst[key] = w
			keys = append(keys, key)
		} else if lag > w.lag {
			w.replica, w.lag = r, lag
		}
		w.count++
	}
	sort.Strings(keys)
	var findings []Finding
	for _, key := range keys {
		if w := worst[key]; w.lag >= 30*time.Second {
			findings = append(findings, replicationLagFinding(key, w))
		}
	}
	return findings
}

// replicaLag is the worst replay lag seen under one replica identity.
type replicaLag struct {
	replica collector.ReplicaInfo
	lag     time.Duration
	count   int
}

// replicaIdentity is "replica:<application_name>@<client_addr>".
func replicaIdentity(r collector.ReplicaInfo) string {
	app := r.ApplicationName
	if app == "" {
		app = "<unnamed>"
	}
	return "replica:" + app + "@" + replicaAddr(r)
}

func replicaAddr(r collector.ReplicaInfo) string {
	if r.ClientAddr != nil {
		return *r.ClientAddr
	}
	return "<unknown>"
}

func replicationLagFinding(ident string, w *replicaLag) Finding {
	r := w.replica
	severity := "warning"
	if w.lag >= 5*time.Minute {
		severity = "critical"
	}
	clientAddr := replicaAddr(r)
	writeLag, flushLag := "", ""
	if r.WriteLag != nil {
		writeLag = *r.WriteLag
	}
	if r.FlushLag != nil {
		flushLag = *r.FlushLag
	}
	return Finding{
		Category:         "replication_lag",
		Severity:         severity,
		ObjectType:       "replication",
		ObjectIdentifier: ident,
		Title: fmt.Sprintf(
			"Replication lag %s on replica %s", *r.ReplayLag, clientAddr,
		),
		Detail: map[string]any{
			"application_name": r.ApplicationName,
			"client_addr":      clientAddr,
			"replay_lag":       *r.ReplayLag,
			"lag_seconds":      w.lag.Seconds(),
			"write_lag":        writeLag,
			"flush_lag":        flushLag,
			"state":            r.State,
			"merged_replicas":  w.count,
		},
		Recommendation: "Investigate replica performance or network issues.",
		ActionRisk:     "safe",
	}
}

// humanBytes formats bytes as a human-readable string.
func humanBytes(b int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
		tb = 1 << 40
	)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(tb))
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// parsePGInterval parses a PostgreSQL interval string like "00:01:23.456"
// or "1 day 02:03:04" into a time.Duration.
func parsePGInterval(s string) time.Duration {
	re := regexp.MustCompile(
		`(?:(\d+)\s+days?\s+)?(\d+):(\d+):(\d+(?:\.\d+)?)`,
	)
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0
	}

	var d time.Duration
	if m[1] != "" {
		days, _ := strconv.Atoi(m[1])
		d += time.Duration(days) * 24 * time.Hour
	}
	hours, _ := strconv.Atoi(m[2])
	mins, _ := strconv.Atoi(m[3])
	secs, _ := strconv.ParseFloat(m[4], 64)

	d += time.Duration(hours) * time.Hour
	d += time.Duration(mins) * time.Minute
	d += time.Duration(secs * float64(time.Second))

	return d
}
