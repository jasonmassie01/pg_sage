package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/pg-sage/sidecar/internal/managedparam"
)

// Parameter is one server parameter as ARM describes it.
type Parameter struct {
	Name           string
	Value          string
	Default        string
	Unit           string
	Source         string // user-override | system-default
	ReadOnly       bool
	Dynamic        bool
	PendingRestart bool
}

// readPermission is the RBAC action Describe needs.
const readPermission = "Microsoft.DBforPostgreSQL/flexibleServers/configurations/read"

// Describe reads one server parameter. It only ever GETs: a 403 names the
// permission to grant, a 404 means the server has no such parameter.
func (a *ParameterAdapter) Describe(ctx context.Context, name string) (Parameter, error) {
	if !parameterPattern.MatchString(name) {
		return Parameter{}, fmt.Errorf("azure: invalid parameter name %q", name)
	}
	var resource configResource
	if _, err := a.do(ctx, http.MethodGet, a.parameterURL(name), nil, &resource); err != nil {
		var status *statusError
		if errors.As(err, &status) && status.Status == http.StatusForbidden {
			return Parameter{}, fmt.Errorf("azure: read parameter %s: %w; the identity "+
				"needs %s on server %s (the Reader role grants it)", name, err,
				readPermission, a.server.Name)
		}
		return Parameter{}, fmt.Errorf("azure: read parameter %s: %w", name, err)
	}
	c := resource.Properties
	return Parameter{Name: name, Value: c.Value, Default: c.DefaultValue, Unit: c.Unit,
		Source: c.Source, ReadOnly: c.IsReadOnly, Dynamic: c.IsDynamicConfig,
		PendingRestart: c.IsConfigPendingRestart}, nil
}

// Target is the server without any parameter described
// (managedparam.Resolver); the managed-change worker calls TargetFor.
func (a *ParameterAdapter) Target(ctx context.Context) (managedparam.Target, error) {
	return a.TargetFor(ctx, nil)
}

// TargetFor describes the named parameters for managed-change proposals
// (managedparam.ParameterResolver). A parameter the server does not have
// (404) is left undescribed; any other failure fails the whole target.
func (a *ParameterAdapter) TargetFor(ctx context.Context,
	names []string) (managedparam.Target, error) {
	t := managedparam.Target{Provider: "azure", Subscription: a.server.SubscriptionID,
		ResourceGroup: a.server.ResourceGroup, InstanceID: a.server.Name, Known: true,
		Params: make(map[string]managedparam.GroupParam, len(names))}
	for _, name := range names {
		p, err := a.Describe(ctx, name)
		var status *statusError
		switch {
		case errors.As(err, &status) && status.Status == http.StatusNotFound:
			continue
		case err != nil:
			return managedparam.Target{}, err
		}
		t.Params[name] = groupParam(p)
	}
	return t, nil
}

// groupParam maps an Azure parameter to the managedparam form: a
// user-override is the operator's value, anything else the system's.
func groupParam(p Parameter) managedparam.GroupParam {
	gp := managedparam.GroupParam{Value: p.Value, Default: p.Default, Source: "system",
		Unit: p.Unit, ApplyType: "static", Modifiable: !p.ReadOnly}
	if p.Source == "user-override" {
		gp.Source = "user"
	}
	if p.Dynamic {
		gp.ApplyType = "dynamic"
	}
	return gp
}
