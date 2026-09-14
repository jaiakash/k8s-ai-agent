{{- define "kai.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kai.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "kai.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kai.labels" -}}
app.kubernetes.io/name: {{ include "kai.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: mcp-server
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "kai.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kai.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kai.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kai.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Fail fast on configurations that would deploy something the operator did not
intend, rather than shipping a server with surprising permissions.
*/}}
{{- define "kai.validate" -}}
{{- if and (eq .Values.rbac.scope "namespaced") (not .Values.policy.allowedNamespaces) -}}
{{- fail "rbac.scope=namespaced requires policy.allowedNamespaces to list at least one namespace" -}}
{{- end -}}
{{- if and .Values.policy.allowWrites (gt (len .Values.policy.allowedNamespaces) 0) -}}
{{- /* allowed + writes is a normal, well-scoped setup */ -}}
{{- end -}}
{{- if not (has .Values.server.transport (list "http" "stdio")) -}}
{{- fail (printf "server.transport must be http or stdio, got %q" .Values.server.transport) -}}
{{- end -}}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.server.stateless) -}}
{{- fail "replicaCount > 1 requires server.stateless=true, otherwise MCP sessions break across replicas" -}}
{{- end -}}
{{- end -}}
