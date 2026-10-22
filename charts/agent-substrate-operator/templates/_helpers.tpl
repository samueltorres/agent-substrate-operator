{{- define "agent-substrate-operator.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agent-substrate-operator.labels" -}}
app.kubernetes.io/name: agent-substrate-operator
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "agent-substrate-operator.selectorLabels" -}}
app.kubernetes.io/name: agent-substrate-operator
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
