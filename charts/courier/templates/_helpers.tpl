{{- define "courier.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "courier.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "courier.labels" -}}
app.kubernetes.io/name: {{ include "courier.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "courier.selectorLabels" -}}
app.kubernetes.io/name: {{ include "courier.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
