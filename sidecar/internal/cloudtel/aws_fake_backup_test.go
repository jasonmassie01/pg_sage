package cloudtel

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The fake RDS's backup records (drill-01b): manual instance and cluster
// snapshots, and the instance's automated backups with their restore
// window. An identifier with nothing recorded lists nothing.

// fakeSnapshot is one manual snapshot; a nil created omits
// SnapshotCreateTime (a malformed or still-starting snapshot).
type fakeSnapshot struct {
	created *time.Time
	status  string
}

func snapshotAt(at time.Time) fakeSnapshot {
	return fakeSnapshot{created: &at, status: "available"}
}

// writeBackupRecords answers the three backup-record reads; f.mu is held.
func (f *fakeAWS) writeBackupRecords(w http.ResponseWriter, action string,
	form url.Values) {
	f.recordQueries = append(f.recordQueries, form)
	var list, member string
	var body strings.Builder
	switch action {
	case "DescribeDBSnapshots":
		list, member = "DBSnapshots", "DBSnapshot"
		writeSnapshots(&body, member, f.snapshots["instance:"+form.Get("DBInstanceIdentifier")])
	case "DescribeDBClusterSnapshots":
		list, member = "DBClusterSnapshots", "DBClusterSnapshot"
		writeSnapshots(&body, member, f.snapshots["cluster:"+form.Get("DBClusterIdentifier")])
	default:
		list = "DBInstanceAutomatedBackups"
		body.WriteString(f.autoBackups[form.Get("DbiResourceId")])
	}
	fmt.Fprintf(w, `<%sResponse xmlns="http://rds.amazonaws.com/doc/2014-10-31/">`+
		`<%sResult><%s>%s</%s></%sResult></%sResponse>`, action, action, list,
		body.String(), list, action, action)
}

func writeSnapshots(b *strings.Builder, member string, snaps []fakeSnapshot) {
	for _, s := range snaps {
		fmt.Fprintf(b, "<%s><SnapshotType>manual</SnapshotType><Status>%s</Status>",
			member, s.status)
		if s.created != nil {
			fmt.Fprintf(b, "<SnapshotCreateTime>%s</SnapshotCreateTime>",
				s.created.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(b, "</%s>", member)
	}
}

// automatedBackupXML is one DBInstanceAutomatedBackup with its restore
// window (zero times omit that end).
func automatedBackupXML(status string, earliest, latest time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<DBInstanceAutomatedBackup><Status>%s</Status><RestoreWindow>", status)
	if !earliest.IsZero() {
		fmt.Fprintf(&b, "<EarliestTime>%s</EarliestTime>", earliest.UTC().Format(time.RFC3339))
	}
	if !latest.IsZero() {
		fmt.Fprintf(&b, "<LatestTime>%s</LatestTime>", latest.UTC().Format(time.RFC3339))
	}
	b.WriteString("</RestoreWindow></DBInstanceAutomatedBackup>")
	return b.String()
}
