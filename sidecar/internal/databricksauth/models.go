package databricksauth

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	databricks "github.com/databricks/databricks-sdk-go"
	"github.com/databricks/databricks-sdk-go/service/catalog"
)

// GatewayModel identifies a Unity Catalog model service usable by pg_sage's
// unified Chat Completions client. Native provider services are separate.
type GatewayModel struct {
	ID          string
	Description string
}

// ListModelServices uses the authenticated workspace catalog, not the unsupported
// OpenAI /models route. Endpoint overrides must still name the SDK workspace.
func ListModelServices(ctx context.Context, endpoint, schema string) ([]GatewayModel, error) {
	w, err := workspace()
	if err != nil {
		return nil, fmt.Errorf("configure Databricks model discovery: %w", err)
	}
	return listModelServices(ctx, endpoint, schema, w)
}

func listModelServices(ctx context.Context, endpoint, schema string, w *databricks.WorkspaceClient) ([]GatewayModel, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid Databricks discovery endpoint")
	}
	if err := validateOrigin(target, w.Config.Host); err != nil {
		return nil, err
	}
	if schema == "" {
		schema = "system.ai"
	}
	parts := strings.Split(schema, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(schema, "/\\\t\r\n") {
		return nil, fmt.Errorf("databricks model schema must be catalog.schema")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	services, err := w.AiGateway.ListModelServicesAll(ctx, catalog.ListModelServicesRequest{
		Parent: "schemas/" + schema, PageSize: 200, View: catalog.ListModelServicesRequestViewBasic,
	})
	if err != nil {
		return nil, fmt.Errorf("list Databricks model services: %w", err)
	}
	models := make([]GatewayModel, 0, len(services))
	for _, service := range services {
		chat := false
		for _, apiType := range service.SupportedApiTypes {
			chat = chat || apiType == "mlflow/v1/chat/completions"
		}
		id := strings.TrimPrefix(service.Name, "model-services/")
		if chat && strings.HasPrefix(service.Name, "model-services/"+schema+".") && id != schema+"." {
			models = append(models, GatewayModel{ID: id, Description: service.Comment})
		}
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}
