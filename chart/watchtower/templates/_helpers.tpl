{{/*
watchtower chart — shared helpers
*/}}
{{- define "watchtower.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "watchtower.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- printf "%s" $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "watchtower.labels" -}}
app.kubernetes.io/name: {{ include "watchtower.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{- define "watchtower.controllerName" -}}
{{ include "watchtower.fullname" . }}-controller
{{- end }}

{{- define "watchtower.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "watchtower.controllerName" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Istio integration mode (D-036/D-037): "auto" | "true" | "false".
A boolean false must not fall back to "auto" (so no `default` here).
*/}}
{{- define "watchtower.istioMode" -}}
{{- $mode := "auto" -}}
{{- with .Values.integrations -}}
{{- with .istio -}}
{{- if and (hasKey . "enabled") (not (kindIs "invalid" .enabled)) -}}
{{- $mode = toString .enabled | lower -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $mode -}}
{{- end }}
