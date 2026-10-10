package decommission

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Logger receives the startup report.
type Logger struct {
	Info, Warn func(format string, args ...any)
}

// Startup runs the §12 startup steps on the control database: it warns
// while provider credentials remain set (step 3), records the YAML
// acknowledgement (step 4), and logs every inventoried item with its delete
// template (step 2). It deletes nothing. A clean install logs nothing.
func Startup(ctx context.Context, pool *pgxpool.Pool, configPath string, log Logger) error {
	if pool == nil {
		return errors.New("decommission inventory: no control database pool")
	}
	inv, err := Build(ctx, pool)
	if err != nil {
		return err
	}
	if names := setCredentialNames(inv.Credentials); len(names) > 0 {
		log.Warn("credentials of the removed AgentDB provisioner are still set: %s. "+
			"Unset them and revoke or rotate them at the provider (%s step 3; "+
			"the inventory at GET %s lists each one)",
			strings.Join(names, ", "), SpecRef, InventoryPath)
	}
	if !inv.LegacyTables {
		return nil
	}
	if applyConfigAck(ctx, pool, configPath, inv, log) {
		if inv, err = Build(ctx, pool); err != nil {
			return err
		}
	}
	logInventory(inv, log)
	return nil
}

// applyConfigAck records the YAML acknowledgement of known items and warns
// about the rest. It reports whether anything new was recorded.
func applyConfigAck(ctx context.Context, pool *pgxpool.Pool, configPath string,
	inv Inventory, log Logger) bool {
	req, err := readConfigAck(configPath)
	if err != nil {
		log.Warn("decommission acknowledgement not read: %v", err)
		return false
	}
	if req == nil {
		return false
	}
	if missing := unknownIDs(inv, req.AcknowledgedResources); len(missing) > 0 {
		log.Warn("%s names ids that are not in the inventory, ignored: %s",
			AckSection, strings.Join(missing, ", "))
		req.AcknowledgedResources = knownIDs(inv, req.AcknowledgedResources)
	}
	if len(req.AcknowledgedResources) == 0 {
		return false
	}
	res, err := Acknowledge(ctx, pool, *req, "config:"+configPath, SourceYAML)
	if err != nil {
		log.Warn("%s not recorded: %v", AckSection, err)
		return false
	}
	if len(res.Acknowledged) > 0 {
		log.Info("decommission: recorded the config acknowledgement of %s",
			strings.Join(res.Acknowledged, ", "))
	}
	return len(res.Acknowledged) > 0
}

func knownIDs(inv Inventory, ids []string) []string {
	missing := map[string]bool{}
	for _, id := range unknownIDs(inv, ids) {
		missing[id] = true
	}
	var out []string
	for _, id := range ids {
		if !missing[id] {
			out = append(out, id)
		}
	}
	return out
}

func logInventory(inv Inventory, log Logger) {
	log.Info("AgentDB decommission (%s): %d resources the removed provisioner may have "+
		"created, %d unacknowledged; pg_sage deletes none of them. Review GET %s",
		SpecRef, len(inv.Items), inv.Unacknowledged, InventoryPath)
	for _, it := range inv.Items {
		log.Info("decommission item %s: provider=%s resource_id=%q deterministic_name=%q "+
			"region=%s account=%s created_at=%s status=%s acknowledged=%v evidence=%s "+
			"delete: %s", it.ID, it.Provider, it.ResourceID, it.DeterministicName,
			it.Region, it.Account, it.CreatedAt.UTC().Format(time.RFC3339), it.Status,
			it.Acknowledged, strings.Join(it.Evidence, ","), templateLine(it.DeleteTemplate))
	}
	if inv.Unacknowledged > 0 {
		log.Warn("%d AgentDB resources are unacknowledged: delete or keep each one by "+
			"hand, then acknowledge them (POST %s, or %s: in the config) per %s",
			inv.Unacknowledged, AckPath, AckSection, SpecRef)
	}
}
