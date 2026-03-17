{{/*
Expand the name of the chart.
*/}}
{{- define "openstack-hypervisor-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "openstack-hypervisor-operator.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "openstack-hypervisor-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "openstack-hypervisor-operator.labels" -}}
helm.sh/chart: {{ include "openstack-hypervisor-operator.chart" . }}
{{ include "openstack-hypervisor-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "openstack-hypervisor-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "openstack-hypervisor-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "openstack-hypervisor-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "openstack-hypervisor-operator.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Additional labels injected into every Prometheus alert rule body.
*/}}
{{- define "openstack-hypervisor-operator.additionalRuleLabels" -}}
{{- with .Values.prometheusRules.additionalRuleLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels applied to PrometheusRule metadata for Prometheus Operator discovery.
*/}}
{{- define "openstack-hypervisor-operator.ruleSelectorLabels" -}}
{{- $root := index . 1 -}}
{{- with $root.Values.prometheusRules.ruleSelectors }}
{{- range $i, $target := . }}
{{ $target.name | required (printf "$.Values.prometheusRules.ruleSelectors[%v].name missing" $i) }}: {{ tpl ($target.value | required (printf "$.Values.prometheusRules.ruleSelectors[%v].value missing" $i)) $root }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Selector labels applied to dashboard ConfigMap metadata for Perses discovery.
*/}}
{{- define "openstack-hypervisor-operator.dashboardSelectorLabels" -}}
{{- $root := index . 1 -}}
{{- with $root.Values.dashboards.dashboardSelectors }}
{{- range $i, $target := . }}
{{ $target.name | required (printf "$.Values.dashboards.dashboardSelectors[%v].name missing" $i) }}: {{ tpl ($target.value | required (printf "$.Values.dashboards.dashboardSelectors[%v].value missing" $i)) $root }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Selector labels applied to global dashboard ConfigMap metadata.
*/}}
{{- define "openstack-hypervisor-operator.globalDashboardSelectorLabels" -}}
{{- $root := index . 1 -}}
{{- with $root.Values.dashboards.global.dashboardSelectors }}
{{- range $i, $target := . }}
{{ $target.name | required (printf "$.Values.dashboards.global.dashboardSelectors[%v].name missing" $i) }}: {{ tpl ($target.value | required (printf "$.Values.dashboards.global.dashboardSelectors[%v].value missing" $i)) $root }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Common labels for monitoring resources (alerts, dashboards).
*/}}
{{- define "openstack-hypervisor-operator.monitoringLabels" -}}
{{- $root := index . 1 -}}
app.kubernetes.io/version: {{ $root.Chart.Version }}
app.kubernetes.io/part-of: {{ $root.Release.Name }}
{{- with $root.Values.global.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}
