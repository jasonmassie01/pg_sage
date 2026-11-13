{{/* Chart name, truncated to the 63-character name limit. */}}
{{- define "pg-sage.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Full name of every object: the release name, plus the chart name
unless the release name already holds it. */}}
{{- define "pg-sage.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains (include "pg-sage.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "pg-sage.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pg-sage.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pg-sage.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "pg-sage.labels" -}}
{{ include "pg-sage.selectorLabels" . }}
app.kubernetes.io/version: {{ include "pg-sage.imageTag" . | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{/* The image tag: values, else the appVersion. "latest" is refused. */}}
{{- define "pg-sage.imageTag" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag | toString -}}
{{- if eq $tag "latest" -}}
{{- fail "image.tag: latest is not allowed; pin a release (default: the chart appVersion)" -}}
{{- end -}}
{{- $tag -}}
{{- end -}}

{{- define "pg-sage.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "pg-sage.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "pg-sage.secretName" -}}
{{- default (printf "%s-secrets" (include "pg-sage.fullname" .)) .Values.secrets.existingSecret -}}
{{- end -}}

{{- define "pg-sage.cnpgNamespace" -}}
{{- default .Release.Namespace .Values.cnpg.namespace -}}
{{- end -}}

{{/* The CNPG Clusters the Role names: cnpg.cluster and cnpg.rbac.clusters. */}}
{{- define "pg-sage.cnpgClusters" -}}
{{- $names := list -}}
{{- if .Values.cnpg.cluster -}}
{{- $names = append $names .Values.cnpg.cluster -}}
{{- end -}}
{{- range .Values.cnpg.rbac.clusters -}}
{{- $names = append $names . -}}
{{- end -}}
{{- toYaml (uniq $names) -}}
{{- end -}}

{{- define "pg-sage.cnpgEnabled" -}}
{{- if or .Values.cnpg.cluster .Values.cnpg.rbac.clusters -}}true{{- end -}}
{{- end -}}

{{- define "pg-sage.scheme" -}}
{{- if .Values.tls.existingSecret -}}HTTPS{{- else -}}HTTP{{- end -}}
{{- end -}}
