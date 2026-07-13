{{- define "akari.name" -}}
{{- default .Chart.Name .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "akari.labels" -}}
app.kubernetes.io/name: {{ include "akari.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "akari.selectorLabels" -}}
app.kubernetes.io/name: {{ include "akari.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
