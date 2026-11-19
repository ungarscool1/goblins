{{- define "goblins.name" -}}{{ .Release.Name | trunc 50 | trimSuffix "-" }}{{- end -}}

{{- define "goblins.workloadNamespace" -}}
{{- default (printf "%s-workloads" .Release.Namespace) .Values.workloads.namespace -}}
{{- end -}}

{{- define "goblins.labels" -}}
app.kubernetes.io/name: goblins
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "goblins.selector" -}}
app.kubernetes.io/name: goblins
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: control-plane
{{- end -}}

{{- define "goblins.restricted" -}}
allowPrivilegeEscalation: false
privileged: false
readOnlyRootFilesystem: true
capabilities: {drop: [ALL]}
{{- end -}}
