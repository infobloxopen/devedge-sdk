{{/*
Expand the name of the chart.
*/}}
{{- define "devedge-service.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "devedge-service.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "devedge-service.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "devedge-service.labels" -}}
helm.sh/chart: {{ include "devedge-service.chart" . }}
{{ include "devedge-service.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "devedge-service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "devedge-service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
The name of the DSN Secret in use (the chart-owned one or a pre-provisioned one).
*/}}
{{- define "devedge-service.dsnSecretName" -}}
{{- if .Values.dsn.existingSecret }}
{{- .Values.dsn.existingSecret }}
{{- else }}
{{- printf "%s-dsn" (include "devedge-service.fullname" .) }}
{{- end }}
{{- end }}
