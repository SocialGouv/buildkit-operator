{{/*
Common naming + label helpers for the buildcat chart.
*/}}

{{- define "buildcat.name" -}}
buildcat
{{- end -}}

{{/*
Fullname: release-qualified, but collapses to "buildcat" when the release is
named buildcat to avoid "buildcat-buildcat".
*/}}
{{- define "buildcat.fullname" -}}
{{- if contains "buildcat" .Release.Name -}}
{{ .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else -}}
{{ printf "%s-%s" .Release.Name "buildcat" | trunc 63 | trimSuffix "-" }}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name: explicit value, else the fullname.
*/}}
{{- define "buildcat.serviceAccountName" -}}
{{- default (include "buildcat.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/*
Standard labels applied to every object.
*/}}
{{- define "buildcat.labels" -}}
app.kubernetes.io/name: {{ include "buildcat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
Selector labels for the buildd workload (component=buildd).
*/}}
{{- define "buildcat.buildd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "buildcat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: buildd
{{- end -}}
