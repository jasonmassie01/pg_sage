package agentdb

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

const maxTerraformTemplateBytes = 256 * 1024

func ValidateTerraformTemplate(req TerraformTemplateRequest) TerraformTemplate {
	template := TerraformTemplate{
		TemplateID: firstNonEmpty(req.TemplateID, "tf_"+idFrom(req.Name)),
		Name:       firstNonEmpty(req.Name, "terraform template"),
		Status:     "draft",
		SourceKind: firstNonEmpty(req.SourceKind, "inline"),
		Files:      sanitizeTerraformFiles(req.Files),
		CreatedBy:  req.CreatedBy,
		Manifest:   map[string]any{},
	}
	template.ContentSHA256 = terraformFilesHash(template.Files)
	template.PolicyFindings = TerraformPolicyFindings(template.Files)
	template.Manifest = TerraformManifest(template.Files)
	if len(template.PolicyFindings) > 0 {
		template.Status = "rejected"
	}
	return template
}

func TerraformPolicyFindings(files []TerraformFile) []string {
	findings := []string{}
	for _, file := range files {
		path := strings.ToLower(filepath.ToSlash(file.Path))
		body := strings.ToLower(file.Body)
		switch {
		case strings.HasSuffix(path, ".tfvars") ||
			strings.HasSuffix(path, ".auto.tfvars") ||
			strings.HasSuffix(path, ".tfstate") ||
			strings.Contains(path, ".terraform/") ||
			sensitiveKey(path):
			findings = append(findings, "secret or state file rejected: "+file.Path)
		case !strings.HasSuffix(path, ".tf") && !strings.HasSuffix(path, ".tf.json"):
			findings = append(findings, "unsupported file type: "+file.Path)
		}
		for _, denied := range []string{
			"provisioner", "local-exec", "remote-exec",
			`data "external"`, `resource "null_resource"`, `resource "local_file"`,
		} {
			if strings.Contains(body, denied) {
				findings = append(findings, "dangerous terraform construct rejected: "+denied)
			}
		}
		if strings.Contains(body, `source = "git::`) && !strings.Contains(body, "?ref=") {
			findings = append(findings, "external module source must pin an immutable ref")
		}
	}
	return findings
}

func TerraformManifest(files []TerraformFile) map[string]any {
	resources := []string{}
	providers := []string{}
	variables := []string{}
	outputs := []string{}
	reResource := regexp.MustCompile(`resource\s+"([^"]+)"\s+"([^"]+)"`)
	reProvider := regexp.MustCompile(`provider\s+"([^"]+)"`)
	reVariable := regexp.MustCompile(`variable\s+"([^"]+)"`)
	reOutput := regexp.MustCompile(`output\s+"([^"]+)"`)
	for _, file := range files {
		body := file.Body
		for _, match := range reResource.FindAllStringSubmatch(body, -1) {
			resources = append(resources, match[1]+"."+match[2])
		}
		for _, match := range reProvider.FindAllStringSubmatch(body, -1) {
			providers = append(providers, match[1])
		}
		for _, match := range reVariable.FindAllStringSubmatch(body, -1) {
			variables = append(variables, match[1])
		}
		for _, match := range reOutput.FindAllStringSubmatch(body, -1) {
			outputs = append(outputs, match[1])
		}
	}
	return map[string]any{
		"providers":  providers,
		"resources":  resources,
		"variables":  variables,
		"outputs":    outputs,
		"file_count": len(files),
	}
}

func TerraformFilesFromZip(name string, data []byte) ([]TerraformFile, error) {
	if len(data) > maxTerraformTemplateBytes {
		return nil, ErrInvalid
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	files := []TerraformFile{}
	for _, f := range reader.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(rc, maxTerraformTemplateBytes+1))
		_ = rc.Close()
		if err != nil || len(body) > maxTerraformTemplateBytes {
			return nil, ErrInvalid
		}
		files = append(files, TerraformFile{Path: f.Name, Body: string(body)})
	}
	if len(files) == 0 {
		return nil, ErrInvalid
	}
	return files, nil
}

func sanitizeTerraformFiles(files []TerraformFile) []TerraformFile {
	out := make([]TerraformFile, 0, len(files))
	for _, file := range files {
		if len(file.Body) > maxTerraformTemplateBytes {
			file.Body = file.Body[:maxTerraformTemplateBytes]
		}
		out = append(out, TerraformFile{Path: file.Path, Body: redactString(file.Body)})
	}
	return out
}

func terraformFilesHash(files []TerraformFile) string {
	h := sha256.New()
	for _, file := range files {
		_, _ = h.Write([]byte(file.Path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(file.Body))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
