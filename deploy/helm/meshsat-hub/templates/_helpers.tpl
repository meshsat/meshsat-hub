{{/*
Common labels
*/}}
{{- define "meshsat-hub.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end }}

{{/*
Selector labels for hub
*/}}
{{- define "meshsat-hub.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: hub
{{- end }}

{{/*
Full name
*/}}
{{- define "meshsat-hub.fullname" -}}
{{ .Release.Name }}-{{ .Chart.Name }}
{{- end }}
