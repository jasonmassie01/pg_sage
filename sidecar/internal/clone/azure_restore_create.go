package clone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// errAzureOpFailed: an ARM operation finished Failed or Canceled.
var errAzureOpFailed = errors.New("the ARM operation failed")

// requireProduction refuses a restore without a production identity
// before Azure is contacted: the reaper's adapter only destroys.
func (a *AzureRestoreAPI) requireProduction() error {
	if a.cfg.Production == nil {
		return fmt.Errorf("%w: azure_restore has no production identity to tell a clone "+
			"from production", ErrCloneUnavailable)
	}
	return nil
}

// azureSource is the monitored server a restore is taken from and the
// DSN that reaches it on its FQDN.
type azureSource struct {
	dsn, host string
	server    azureServer
}

// LatestSnapshot is the source server at AzureRestoreLag ago: a point in
// time of its backups.
func (a *AzureRestoreAPI) LatestSnapshot(ctx context.Context) (Snapshot, error) {
	if err := a.requireProduction(); err != nil {
		return Snapshot{}, err
	}
	api, err := a.client(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, azureCallTimeout)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Snapshot{}, err
	}
	pit := a.cfg.Now().Add(-AzureRestoreLag).UTC().Truncate(time.Second)
	if err := src.restorable(pit); err != nil {
		return Snapshot{}, err
	}
	return Snapshot{ID: src.server.ID, CreatedAt: pit}, nil
}

// restorable refuses a point in time before the earliest restore date.
func (s azureSource) restorable(pit time.Time) error {
	earliest := s.server.Props.Backup.EarliestRestoreDate
	if earliest.IsZero() {
		return fmt.Errorf("%w: Azure reports no earliest restore date for server %s",
			ErrCloneUnavailable, s.server.Name)
	}
	if pit.Before(earliest) {
		return fmt.Errorf("%w: the point in time %s is before server %s's earliest "+
			"restore date %s (the server was created recently)", ErrCloneUnavailable,
			pit.Format(time.RFC3339), s.server.Name, earliest.UTC().Format(time.RFC3339))
	}
	return nil
}

// source reads production's DSN and the configured server, and checks it
// is a ready primary flexible server the DSN reaches on its FQDN:
// pg_sage restores only what it monitors, and reaches the clone the same
// way. Errors never quote the DSN.
func (a *AzureRestoreAPI) source(ctx context.Context, api *azureHTTP) (azureSource, error) {
	prod, err := a.cfg.Production(ctx)
	if err != nil {
		return azureSource{}, fmt.Errorf("%w: read the monitored Azure database's "+
			"identity: %w", ErrCloneUnavailable, err)
	}
	if blank(prod.DSN) {
		return azureSource{}, fmt.Errorf("%w: the monitored Azure database has no DSN",
			ErrCloneUnavailable)
	}
	host, err := dsnHost(prod.DSN)
	if err != nil {
		return azureSource{}, fmt.Errorf("%w: %w", ErrInvalidAzureRestoreConfig, err)
	}
	s, err := api.GetServer(ctx, a.cfg.Server)
	if errors.Is(err, ErrAzureNotFound) {
		return azureSource{}, fmt.Errorf("%w: resource group %s has no flexible server %s: "+
			"%w", ErrNotAzureFlexibleServer, a.cfg.ResourceGroup, a.cfg.Server, err)
	}
	if err != nil {
		return azureSource{}, fmt.Errorf("azure_restore: read the monitored server %s: %w",
			a.cfg.Server, err)
	}
	switch role := s.Props.ReplicationRole; {
	case role != "" && role != "None" && role != "Primary":
		return azureSource{}, fmt.Errorf("%w: server %s is a %s, not a primary; restore "+
			"from the primary", ErrNotAzureFlexibleServer, a.cfg.Server, role)
	case blank(s.Props.FQDN) || !sameHost(host, s.Props.FQDN):
		return azureSource{}, fmt.Errorf("%w: the monitored host %s is not server %s's "+
			"FQDN %s; connect pg_sage to the server's FQDN (not an IP or another name)",
			ErrNotAzureFlexibleServer, host, a.cfg.Server, s.Props.FQDN)
	case s.Props.State != "Ready":
		return azureSource{}, fmt.Errorf("%w: server %s is %s, not Ready",
			ErrCloneUnavailable, a.cfg.Server, s.Props.State)
	case blank(s.Location):
		return azureSource{}, fmt.Errorf("%w: server %s names no region",
			ErrNotAzureFlexibleServer, a.cfg.Server)
	}
	return azureSource{dsn: prod.DSN, host: host, server: s}, nil
}

// Restore restores the monitored server at snap's point in time as
// spec.ResourceName (sage-<purpose>-<8 hex>, the receipt's), tagged with
// the receipt name and expiry, within the restore deadline, and returns
// the monitored DSN on the new server's FQDN. A failure after the server
// exists deletes it before the error returns; a wait cut short leaves it
// to the reaper, which finds it by name.
func (a *AzureRestoreAPI) Restore(ctx context.Context, snap Snapshot,
	spec CloneSpec) (Clone, error) {
	if err := a.requireProduction(); err != nil {
		return Clone{}, err
	}
	if err := callerOutlasts(ctx, a.cfg.Deadline); err != nil {
		return Clone{}, err
	}
	api, err := a.client(ctx)
	if err != nil {
		return Clone{}, err
	}
	if err := checkAzureName(spec.ResourceName, "nothing created"); err != nil {
		return Clone{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Deadline)
	defer cancel()
	src, err := a.source(ctx, api)
	if err != nil {
		return Clone{}, err
	}
	if !strings.EqualFold(snap.ID, src.server.ID) || snap.CreatedAt.After(a.cfg.Now()) {
		return Clone{}, fmt.Errorf("%w: snapshot %q at %s is not a past point in time of "+
			"server %s; nothing created", ErrInvalidAzureRestoreConfig, snap.ID,
			snap.CreatedAt.Format(time.RFC3339), src.server.Name)
	}
	if err := src.restorable(snap.CreatedAt); err != nil {
		return Clone{}, err
	}
	run := &azRestore{a: a, api: api, src: src, name: spec.ResourceName,
		pit: snap.CreatedAt.UTC(), expire: a.cfg.Now().Add(a.cfg.TTL).UTC()}
	return run.restore(ctx)
}

// azRestore is one Restore: what it asked for, so cleanup deletes exactly
// that.
type azRestore struct {
	a           *AzureRestoreAPI
	api         *azureHTTP
	src         azureSource
	name        string
	pit, expire time.Time
}

// body is the new server: a PointInTimeRestore of the source in its
// region, tagged (source tags kept for cost attribution), in the source's
// delegated subnet, under its customer key, without a standby, with the
// shortest backup retention so the clone's own backups outlive it as
// little as Azure allows.
func (r *azRestore) body() map[string]any {
	tags := maps.Clone(r.src.server.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	tags[AzureReceiptTag] = r.name
	tags[AzureExpiresTag] = strconv.FormatInt(r.expire.Unix(), 10)
	props := map[string]any{"createMode": "PointInTimeRestore",
		"sourceServerResourceId": r.src.server.ID,
		"pointInTimeUTC":         r.pit.Format(time.RFC3339),
		"highAvailability":       map[string]string{"mode": "Disabled"},
		"backup":                 map[string]int{"backupRetentionDays": AzureCloneBackupDays}}
	var net struct {
		Subnet  string `json:"delegatedSubnetResourceId"`
		DNSZone string `json:"privateDnsZoneArmResourceId"`
	}
	if json.Unmarshal(r.src.server.Props.Network, &net) == nil && !blank(net.Subnet) {
		props["network"] = map[string]string{"delegatedSubnetResourceId": net.Subnet,
			"privateDnsZoneArmResourceId": net.DNSZone}
	}
	body := map[string]any{"location": r.src.server.Location, "tags": tags,
		"properties": props}
	var enc struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(r.src.server.Props.DataEncryption, &enc) == nil &&
		enc.Type == "AzureKeyVault" {
		props["dataEncryption"] = r.src.server.Props.DataEncryption
		body["identity"] = r.src.server.Identity
	}
	return body
}

func (r *azRestore) restore(ctx context.Context) (Clone, error) {
	// ARM's PUT is create-or-update: never send one to an existing server.
	_, err := r.api.GetServer(ctx, r.name)
	if err == nil {
		return Clone{}, fmt.Errorf("%w: a server named %s already exists in resource group "+
			"%s; nothing created", ErrAzureAlreadyExists, r.name, r.a.cfg.ResourceGroup)
	}
	if !errors.Is(err, ErrAzureNotFound) {
		return Clone{}, fmt.Errorf("azure_restore: check server name %s is free: %w",
			r.name, err)
	}
	poll, err := r.api.PutServer(ctx, r.name, r.body())
	if err != nil {
		err = fmt.Errorf("azure_restore: restore %s to %s: %w", r.src.server.Name, r.name,
			err)
		// ARM refused it (4xx, a name in use included): nothing was made.
		// A 5xx or a lost reply may follow a PUT it accepted.
		var status *ProviderStatusError
		if errors.As(err, &status) && status.StatusCode < 500 {
			return Clone{}, err
		}
		return Clone{}, r.cleanupByName(err)
	}
	if err := r.wait(ctx, poll); err != nil {
		return Clone{}, err
	}
	return r.ready(ctx)
}

// ready reads the restored server: Ready, tagged for this receipt, on an
// FQDN that is not production's.
func (r *azRestore) ready(ctx context.Context) (Clone, error) {
	s, err := r.api.GetServer(ctx, r.name)
	if err == nil && s.Props.State != "Ready" {
		err = fmt.Errorf("server %s is %s after its restore finished", r.name, s.Props.State)
	}
	if err != nil {
		return Clone{}, r.cleanup(fmt.Errorf("azure_restore: read the clone: %w", err))
	}
	if s.Tags[AzureReceiptTag] != r.name {
		// Never delete what pg_sage cannot prove is its own; the reaper
		// will not find it either, so the operator must.
		return Clone{}, fmt.Errorf("%w: azure_restore: restored server %s does not carry "+
			"the tag %s=%s, so pg_sage cannot prove it is its own and did not delete it; "+
			"%s", ErrCloneLeftBehind, r.name, AzureReceiptTag, r.name, r.a.byHand(r.name))
	}
	dsn, err := r.dsn(s)
	if err != nil {
		return Clone{}, r.cleanup(err)
	}
	return Clone{DSN: dsn, ID: r.name, CreatedFrom: r.pit}, nil
}

// dsn is production's DSN on the clone's FQDN, checked not to be
// production's.
func (r *azRestore) dsn(s azureServer) (string, error) {
	host := s.Props.FQDN
	if blank(host) {
		return "", fmt.Errorf("azure_restore: clone %s has no FQDN", r.name)
	}
	if sameHost(host, r.src.host) || sameHost(host, r.src.server.Props.FQDN) {
		return "", fmt.Errorf("%w: clone %s answers on production's FQDN %s",
			ErrCloneIsProduction, r.name, host)
	}
	dsn, err := auroraCloneDSN(r.src.dsn, r.src.host, host, 5432)
	if err != nil {
		return "", fmt.Errorf("azure_restore: %w", err)
	}
	return dsn, nil
}
