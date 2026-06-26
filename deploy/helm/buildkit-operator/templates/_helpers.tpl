{{/*
Common naming + label helpers for the buildkit-operator chart.
*/}}

{{- define "buildkit-operator.name" -}}
buildkit-operator
{{- end -}}

{{/*
Fullname: release-qualified, but collapses to "buildkit-operator" when the release is
named buildkit-operator to avoid "buildkit-operator-buildkit-operator".
*/}}
{{- define "buildkit-operator.fullname" -}}
{{- if contains "buildkit-operator" .Release.Name -}}
{{ .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else -}}
{{ printf "%s-%s" .Release.Name "buildkit-operator" | trunc 63 | trimSuffix "-" }}
{{- end -}}
{{- end -}}

{{/*
Namespaces. Three-namespace topology, split by trust/role so each carries only the Kyverno exemption
its workloads need:
  - operator: the control plane (buildd + gateway). Hardened; needs NO privileged exemption.
  - builds:   the per-project build daemons (+ untrusted forks) and their certs/config/mirror. Needs
              the securityContext exemption (rootless relaxes allowPrivilegeEscalation).
(The Kata node plumbing lives in a third namespace, buildkit-system, installed out-of-band — see
deploy/kata/, not this chart.)
*/}}
{{- /* nil-safe: with `helm upgrade --reuse-values`, .Values.namespaces may be absent, so a plain
       .Values.namespaces.operator would nil-pointer (and `and`/`dig` don't help here — `and` isn't
       short-circuit and `dig` rejects the common.Values type). Nested `with` only descends when each
       level is present. */ -}}
{{- define "buildkit-operator.operatorNamespace" -}}
{{- $ns := "buildkit-operator" -}}
{{- with .Values.namespaces }}{{- with .operator }}{{- $ns = . }}{{- end }}{{- end -}}
{{- $ns -}}
{{- end -}}
{{- define "buildkit-operator.buildsNamespace" -}}
{{- $ns := "buildkit-builds" -}}
{{- with .Values.namespaces }}{{- with .builds }}{{- $ns = . }}{{- end }}{{- end -}}
{{- $ns -}}
{{- end -}}

{{/*
ServiceAccount name: explicit value, else the fullname.
*/}}
{{- define "buildkit-operator.serviceAccountName" -}}
{{- default (include "buildkit-operator.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/*
Standard labels applied to every object.
*/}}
{{- define "buildkit-operator.labels" -}}
app.kubernetes.io/name: {{ include "buildkit-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/*
Selector labels for the buildd workload (component=buildd).
*/}}
{{- define "buildkit-operator.buildd.selectorLabels" -}}
app.kubernetes.io/name: {{ include "buildkit-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: buildd
{{- end -}}
