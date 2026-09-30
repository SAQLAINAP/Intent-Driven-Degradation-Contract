{{- define "dg.fullname" -}}
{{- printf "%s-engine" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "dg.labels" -}}
app.kubernetes.io/name: dg-engine
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "dg.selector" -}}
app.kubernetes.io/name: dg-engine
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
