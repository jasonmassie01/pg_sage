package cloudtel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Cloud SQL Admin API v1 reads: the instance (tier, storage, auto-resize,
// replicas, database flags) and, when only the IP is known, the instance
// list to match it.

type sqlInstance struct {
	Name         string   `json:"name"`
	ReplicaNames []string `json:"replicaNames"`
	IPAddresses  []struct {
		IPAddress string `json:"ipAddress"`
	} `json:"ipAddresses"`
	Settings struct {
		Tier                   string          `json:"tier"`
		StorageAutoResize      bool            `json:"storageAutoResize"`
		StorageAutoResizeLimit string          `json:"storageAutoResizeLimit"`
		BackupConfiguration    *cloudSQLBackup `json:"backupConfiguration"`
		DeletionProtection     *bool           `json:"deletionProtectionEnabled"`
		DatabaseFlags          []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"databaseFlags"`
	} `json:"settings"`
}

func (i sqlInstance) replicaIDs(project string) []string {
	var out []string
	for _, r := range i.ReplicaNames {
		if gcpInstancePattern.MatchString(r) && len(out) < 20 {
			out = append(out, project+":"+r)
		}
	}
	return out
}

// instance reads the named instance, or finds it by IP once and
// remembers its name.
func (s *GCPSource) instance(ctx context.Context) (sqlInstance, error) {
	s.mu.Lock()
	name := s.name
	s.mu.Unlock()
	base := s.opts.SQLAdminEndpoint + "/v1/projects/" + url.PathEscape(s.opts.Project) +
		"/instances"
	if name == "" {
		found, err := s.findByIP(ctx, base)
		if err != nil {
			return sqlInstance{}, err
		}
		s.mu.Lock()
		s.name = found.Name
		s.mu.Unlock()
		return found, nil
	}
	raw, err := s.get(ctx, base+"/"+url.PathEscape(name), "Cloud SQL Admin instances.get")
	if err != nil {
		return sqlInstance{}, err
	}
	var inst sqlInstance
	if err := json.Unmarshal(raw, &inst); err != nil || inst.Name == "" {
		return sqlInstance{}, fmt.Errorf("%w: Cloud SQL instance JSON", ErrMalformed)
	}
	return inst, nil
}

func (s *GCPSource) findByIP(ctx context.Context, base string) (sqlInstance, error) {
	raw, err := s.get(ctx, base, "Cloud SQL Admin instances.list")
	if err != nil {
		return sqlInstance{}, err
	}
	var list struct {
		Items []sqlInstance `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return sqlInstance{}, fmt.Errorf("%w: Cloud SQL instance list JSON", ErrMalformed)
	}
	want := net.ParseIP(s.opts.HostIP)
	for _, inst := range list.Items {
		for _, ip := range inst.IPAddresses {
			if got := net.ParseIP(ip.IPAddress); got != nil && got.Equal(want) &&
				gcpInstancePattern.MatchString(inst.Name) {
				return inst, nil
			}
		}
	}
	return sqlInstance{}, fmt.Errorf("%w: no Cloud SQL instance in project %s has IP %s; set "+
		"cloud_telemetry.gcp.instance", ErrIdentity, s.opts.Project, s.opts.HostIP)
}

// applyCloudSQLStorage bounds capacity: unlimited auto-resize grows
// without a bound; a resize limit is the bound; otherwise the disk quota.
func applyCloudSQLStorage(s *Sample, inst sqlInstance, now time.Time) {
	limitGB, _ := strconv.ParseFloat(inst.Settings.StorageAutoResizeLimit, 64)
	switch {
	case inst.Settings.StorageAutoResize && limitGB <= 0:
		s.StorageAutoGrows = true
		s.StorageCapacityBytes = nil
	case inst.Settings.StorageAutoResize:
		s.StorageCapacityBytes = &Point{Value: limitGB * gibF, At: now}
	case s.AllocatedStorageBytes != nil:
		c := *s.AllocatedStorageBytes
		s.StorageCapacityBytes = &c
	}
}

var (
	customTier   = regexp.MustCompile(`^db-custom-([0-9]+)-([0-9]+)$`)
	standardTier = regexp.MustCompile(`^db-n1-(standard|highmem)-([0-9]+)$`)
)

// sharedCoreTierMemory: shared-core tiers have fixed RAM (db-f1-micro as
// Cloud Monitoring reports its memory/quota; db-g1-small 1.7 GB).
var sharedCoreTierMemory = map[string]float64{
	"db-f1-micro": 643825664,
	"db-g1-small": 1.7e9,
}

// tierMemoryBytes is the RAM of a Cloud SQL machine tier (0: unknown).
func tierMemoryBytes(tier string) float64 {
	if mem, ok := sharedCoreTierMemory[tier]; ok {
		return mem
	}
	if m := customTier.FindStringSubmatch(tier); m != nil {
		mb, _ := strconv.ParseFloat(m[2], 64)
		return mb * (1 << 20)
	}
	if m := standardTier.FindStringSubmatch(tier); m != nil {
		cpus, _ := strconv.ParseFloat(m[2], 64)
		perCPU := 3.75
		if m[1] == "highmem" {
			perCPU = 6.5
		}
		return cpus * perCPU * gibF
	}
	return 0
}
