package api

import (
	"encoding/base64"
	"net/http"

	"github.com/pg-sage/sidecar/internal/agentdb"
)

func agentDBTerraformTemplatesHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := st.TerraformTemplates(r.Context())
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, map[string]any{"terraform_templates": rows})
	}
}

func agentDBCreateTerraformTemplateHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		files := terraformFilesFromBody(m)
		if zip64 := str(m, "zip_base64"); zip64 != "" {
			data, err := base64.StdEncoding.DecodeString(zip64)
			if err != nil {
				agentDBError(w, agentdb.ErrInvalid)
				return
			}
			files, err = agentdb.TerraformFilesFromZip(str(m, "name"), data)
			if err != nil {
				agentDBError(w, agentdb.ErrInvalid)
				return
			}
		}
		template, err := st.CreateTerraformTemplate(
			r.Context(),
			agentdb.TerraformTemplateRequest{
				TemplateID: str(m, "template_id"),
				Name:       str(m, "name"),
				SourceKind: firstString(str(m, "source_kind"), "inline"),
				Files:      files,
				CreatedBy:  str(m, "created_by"),
			},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, template)
	}
}

func agentDBApproveTerraformTemplateHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		template, err := st.ApproveTerraformTemplate(
			r.Context(), r.PathValue("template_id"), str(m, "approved_by"),
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, template)
	}
}

func agentDBProvisionTerraformTemplateHandler(st *agentdb.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := readMap(r)
		dep, err := st.ProvisionFromTerraformTemplate(
			r.Context(),
			r.PathValue("template_id"),
			agentdb.TemplateProvisionRequest{
				DeploymentID:      str(m, "deployment_id"),
				TenantID:          str(m, "tenant_id"),
				AgentID:           str(m, "agent_id"),
				RunID:             str(m, "run_id"),
				DatabaseName:      str(m, "database_name"),
				Provider:          str(m, "provider"),
				ProvisioningLevel: str(m, "provisioning_level"),
				LeaseSeconds:      integer(m, "lease_seconds"),
				BudgetUSD:         float(m, "budget_usd"),
				Metadata:          obj(m, "metadata"),
				ProviderParams:    obj(m, "provider_params"),
			},
		)
		if err != nil {
			agentDBError(w, err)
			return
		}
		jsonResponse(w, dep)
	}
}

func terraformFilesFromBody(m map[string]any) []agentdb.TerraformFile {
	raw, ok := m["files"].([]any)
	if !ok {
		return nil
	}
	out := make([]agentdb.TerraformFile, 0, len(raw))
	for _, item := range raw {
		file, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, agentdb.TerraformFile{
			Path: str(file, "path"),
			Body: str(file, "body"),
		})
	}
	return out
}
