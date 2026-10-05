package cloudtel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// fakeAWS is one httptest server standing in for CloudWatch (Query
// GetMetricData), Performance Insights (JSON 1.1 GetResourceMetrics) and
// RDS (Query DescribeDB*). No test ever reaches a real AWS endpoint.

const (
	testAccessKey = "AKIDTESTONLY0000"
	testSecretKey = "secret-never-logged-0123456789"
	testToken     = "session-token-never-logged"
)

type fakePoint struct {
	at    time.Time
	value float64
}

type fakeFailure struct {
	status  int
	code    string
	message string
	body    string // raw body override (malformed responses)
}

type fakeParam struct {
	name, value, source, applyType string
	modifiable                     bool
}

type fakeAWS struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	now time.Time

	instances map[string]string // identifier -> DBInstance XML body
	clusters  map[string]string // cluster id -> DBCluster XML body
	params    map[string][]fakeParam
	metrics   map[string]map[string][]fakePoint // instance -> metric -> points
	pi        []piSeries
	fail      map[string]fakeFailure // "cw", "pi", "rds:<Action>"
	dateSkew  time.Duration

	calls      map[string]int
	authHeader []string
	cwQueries  []url.Values
	piRequests []map[string]any
}

type piSeries struct {
	metric string
	wait   string // "" for the ungrouped series
	points []fakePoint
}

func newFakeAWS(t *testing.T, now time.Time) *fakeAWS {
	t.Helper()
	f := &fakeAWS{t: t, now: now, instances: map[string]string{},
		clusters: map[string]string{}, params: map[string][]fakeParam{},
		metrics: map[string]map[string][]fakePoint{}, fail: map[string]fakeFailure{},
		calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAWS) metric(instance, name string, ago time.Duration, value float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.metrics[instance] == nil {
		f.metrics[instance] = map[string][]fakePoint{}
	}
	f.metrics[instance][name] = append(f.metrics[instance][name],
		fakePoint{at: f.now.Add(-ago), value: value})
}

func (f *fakeAWS) called(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[key]
}

func (f *fakeAWS) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.authHeader = append(f.authHeader, r.Header.Get("Authorization"))
	f.mu.Unlock()
	w.Header().Set("Date", f.now.Add(f.dateSkew).UTC().Format(http.TimeFormat))
	if target := r.Header.Get("X-Amz-Target"); target != "" {
		f.servePI(w, target, body)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	switch action := form.Get("Action"); action {
	case "GetMetricData":
		f.serveCloudWatch(w, form)
	default:
		f.serveRDS(w, action, form)
	}
}

func (f *fakeAWS) failed(w http.ResponseWriter, key string, jsonStyle bool) bool {
	f.mu.Lock()
	f.calls[key]++
	fail, ok := f.fail[key]
	f.mu.Unlock()
	if !ok {
		return false
	}
	w.WriteHeader(fail.status)
	switch {
	case fail.body != "":
		_, _ = io.WriteString(w, fail.body)
	case jsonStyle:
		_ = json.NewEncoder(w).Encode(map[string]string{
			"__type": fail.code, "Message": fail.message})
	default:
		fmt.Fprintf(w, `<ErrorResponse><Error><Type>Sender</Type><Code>%s</Code>`+
			`<Message>%s</Message></Error><RequestId>r-1</RequestId></ErrorResponse>`,
			fail.code, fail.message)
	}
	return true
}

func (f *fakeAWS) serveCloudWatch(w http.ResponseWriter, form url.Values) {
	if f.failed(w, "cw", false) {
		return
	}
	f.mu.Lock()
	f.cwQueries = append(f.cwQueries, form)
	f.mu.Unlock()
	var b strings.Builder
	b.WriteString(`<GetMetricDataResponse xmlns="http://monitoring.amazonaws.com/doc/` +
		`2010-08-01/"><GetMetricDataResult><MetricDataResults>`)
	for i := 1; ; i++ {
		prefix := fmt.Sprintf("MetricDataQueries.member.%d.", i)
		id := form.Get(prefix + "Id")
		if id == "" {
			break
		}
		name := form.Get(prefix + "MetricStat.Metric.MetricName")
		instance := form.Get(prefix + "MetricStat.Metric.Dimensions.member.1.Value")
		f.mu.Lock()
		points := append([]fakePoint(nil), f.metrics[instance][name]...)
		f.mu.Unlock()
		sort.Slice(points, func(a, c int) bool { return points[a].at.After(points[c].at) })
		fmt.Fprintf(&b, "<member><Id>%s</Id><Label>%s</Label><StatusCode>Complete"+
			"</StatusCode><Timestamps>", id, name)
		for _, p := range points {
			fmt.Fprintf(&b, "<member>%s</member>", p.at.UTC().Format(time.RFC3339))
		}
		b.WriteString("</Timestamps><Values>")
		for _, p := range points {
			fmt.Fprintf(&b, "<member>%s</member>", strconv.FormatFloat(p.value, 'f', -1, 64))
		}
		b.WriteString("</Values></member>")
	}
	b.WriteString(`</MetricDataResults><Messages/></GetMetricDataResult>` +
		`<ResponseMetadata><RequestId>r-2</RequestId></ResponseMetadata>` +
		`</GetMetricDataResponse>`)
	w.Header().Set("Content-Type", "text/xml")
	_, _ = io.WriteString(w, b.String())
}

func (f *fakeAWS) servePI(w http.ResponseWriter, target string, body []byte) {
	if f.failed(w, "pi", true) {
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.piRequests = append(f.piRequests, req)
	series := append([]piSeries(nil), f.pi...)
	f.mu.Unlock()
	if target != "PerformanceInsightsv20180227.GetResourceMetrics" {
		http.Error(w, "unknown target "+target, http.StatusBadRequest)
		return
	}
	list := []map[string]any{}
	for _, s := range series {
		key := map[string]any{"Metric": s.metric}
		if s.wait != "" {
			key["Dimensions"] = map[string]string{"db.wait_event_type.name": s.wait}
		}
		points := []map[string]any{}
		for _, p := range s.points {
			points = append(points, map[string]any{
				"Timestamp": float64(p.at.Unix()), "Value": p.value})
		}
		list = append(list, map[string]any{"Key": key, "DataPoints": points})
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"Identifier": req["Identifier"], "MetricList": list})
}

func (f *fakeAWS) serveRDS(w http.ResponseWriter, action string, form url.Values) {
	if f.failed(w, "rds:"+action, false) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "text/xml")
	switch action {
	case "DescribeDBInstances":
		inst, ok := f.instances[form.Get("DBInstanceIdentifier")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>`+
				`DBInstanceNotFound</Code><Message>not found</Message></Error></ErrorResponse>`)
			return
		}
		fmt.Fprintf(w, `<DescribeDBInstancesResponse xmlns="http://rds.amazonaws.com/doc/`+
			`2014-10-31/"><DescribeDBInstancesResult><DBInstances>%s</DBInstances>`+
			`</DescribeDBInstancesResult></DescribeDBInstancesResponse>`, inst)
	case "DescribeDBClusters":
		cl, ok := f.clusters[form.Get("DBClusterIdentifier")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `<ErrorResponse><Error><Type>Sender</Type><Code>`+
				`DBClusterNotFoundFault</Code><Message>nf</Message></Error></ErrorResponse>`)
			return
		}
		fmt.Fprintf(w, `<DescribeDBClustersResponse xmlns="http://rds.amazonaws.com/doc/`+
			`2014-10-31/"><DescribeDBClustersResult><DBClusters>%s</DBClusters>`+
			`</DescribeDBClustersResult></DescribeDBClustersResponse>`, cl)
	case "DescribeDBParameters":
		f.writeParams(w, form)
	default:
		http.Error(w, "unknown action "+action, http.StatusBadRequest)
	}
}

// writeParams pages the group's parameters two at a time (Marker = offset).
func (f *fakeAWS) writeParams(w http.ResponseWriter, form url.Values) {
	all := f.params[form.Get("DBParameterGroupName")]
	start, _ := strconv.Atoi(form.Get("Marker"))
	end := min(start+2, len(all))
	var b strings.Builder
	for _, p := range all[start:end] {
		fmt.Fprintf(&b, "<Parameter><ParameterName>%s</ParameterName>", p.name)
		if p.value != "" {
			fmt.Fprintf(&b, "<ParameterValue>%s</ParameterValue>", p.value)
		}
		fmt.Fprintf(&b, "<Source>%s</Source><ApplyType>%s</ApplyType>"+
			"<IsModifiable>%t</IsModifiable></Parameter>", p.source, p.applyType, p.modifiable)
	}
	marker := ""
	if end < len(all) {
		marker = "<Marker>" + strconv.Itoa(end) + "</Marker>"
	}
	fmt.Fprintf(w, `<DescribeDBParametersResponse xmlns="http://rds.amazonaws.com/doc/`+
		`2014-10-31/"><DescribeDBParametersResult><Parameters>%s</Parameters>%s`+
		`</DescribeDBParametersResult></DescribeDBParametersResponse>`, b.String(), marker)
}

// instanceXML renders one DBInstance element.
func instanceXML(id, class, engine, group, applyStatus string, allocatedGiB, maxGiB int,
	pi bool, cluster string, replicas ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<DBInstance><DBInstanceIdentifier>%s</DBInstanceIdentifier>"+
		"<DBInstanceClass>%s</DBInstanceClass><Engine>%s</Engine>"+
		"<DbiResourceId>db-%s</DbiResourceId><AllocatedStorage>%d</AllocatedStorage>",
		id, class, engine, strings.ToUpper(id), allocatedGiB)
	if maxGiB > 0 {
		fmt.Fprintf(&b, "<MaxAllocatedStorage>%d</MaxAllocatedStorage>", maxGiB)
	}
	fmt.Fprintf(&b, "<PerformanceInsightsEnabled>%t</PerformanceInsightsEnabled>", pi)
	if cluster != "" {
		fmt.Fprintf(&b, "<DBClusterIdentifier>%s</DBClusterIdentifier>", cluster)
	}
	fmt.Fprintf(&b, "<DBParameterGroups><DBParameterGroup><DBParameterGroupName>%s"+
		"</DBParameterGroupName><ParameterApplyStatus>%s</ParameterApplyStatus>"+
		"</DBParameterGroup></DBParameterGroups>", group, applyStatus)
	b.WriteString("<ReadReplicaDBInstanceIdentifiers>")
	for _, r := range replicas {
		fmt.Fprintf(&b, "<ReadReplicaDBInstanceIdentifier>%s</ReadReplicaDBInstanceIdentifier>",
			r)
	}
	b.WriteString("</ReadReplicaDBInstanceIdentifiers></DBInstance>")
	return b.String()
}

func clusterXML(id, writer string, readers ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<DBCluster><DBClusterIdentifier>%s</DBClusterIdentifier>"+
		"<Engine>aurora-postgresql</Engine><DBClusterMembers>", id)
	fmt.Fprintf(&b, "<DBClusterMember><DBInstanceIdentifier>%s</DBInstanceIdentifier>"+
		"<IsClusterWriter>true</IsClusterWriter></DBClusterMember>", writer)
	for _, r := range readers {
		fmt.Fprintf(&b, "<DBClusterMember><DBInstanceIdentifier>%s</DBInstanceIdentifier>"+
			"<IsClusterWriter>false</IsClusterWriter></DBClusterMember>", r)
	}
	b.WriteString("</DBClusterMembers></DBCluster>")
	return b.String()
}

func staticCreds() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey,
			SessionToken: testToken, Source: "test"}, nil
	})
}

func failingCreds() aws.CredentialsProvider {
	return aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{}, errors.New("no EC2 IMDS role found, " +
			"operation error ec2imds: GetMetadata")
	})
}

// awsSource builds a source against the fake for one instance.
func (f *fakeAWS) source(t *testing.T, opts AWSOptions) *AWSSource {
	t.Helper()
	if opts.Region == "" {
		opts.Region = "us-east-1"
	}
	if opts.Credentials == nil {
		opts.Credentials = staticCreds()
	}
	opts.CloudWatchEndpoint, opts.PIEndpoint, opts.RDSEndpoint = f.srv.URL, f.srv.URL,
		f.srv.URL
	opts.Now = func() time.Time { return f.now }
	src, err := NewAWSSource(opts)
	if err != nil {
		t.Fatalf("NewAWSSource: %v", err)
	}
	return src
}
