{{/*
Name prefix for every resource this chart renders: the release name, truncated
to the DNS label limit. With the documented install command
(`helm install kvsynk8s ... --namespace kvsynk8s`) every rendered name is
byte-identical to install.yaml, e.g. kvsynk8s-operator. Installing under a
different release name keeps the cluster-scoped RBAC objects collision-free.
See specs/002-helm-chart/research.md R10.
*/}}
{{- define "kvsynk8s.prefix" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Build "<prefix>-<suffix>" and keep it inside the 63-character DNS limit.
Usage: {{ include "kvsynk8s.name" (dict "ctx" . "suffix" "operator") }}
*/}}
{{- define "kvsynk8s.name" -}}
{{- printf "%s-%s" (include "kvsynk8s.prefix" .ctx) .suffix | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels. Applied to every resource.
*/}}
{{- define "kvsynk8s.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: kvsynk8s
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Labels for the resources that are part of, or select, the manager Pod: the
common labels plus control-plane, which kustomize also puts on the Deployment,
the metrics Service and the ServiceMonitor. Kept separate from
"kvsynk8s.labels" so app.kubernetes.io/name is never emitted twice.
*/}}
{{- define "kvsynk8s.workloadLabels" -}}
{{ include "kvsynk8s.labels" . }}
control-plane: controller-manager
{{- end -}}

{{/*
Selector labels. These are deliberately free of release-specific values and
identical to what kustomize emits, so the metrics Service and the NetworkPolicy
keep selecting the manager Pod exactly as install.yaml does. They are also
immutable on a Deployment, so they must never gain a templated component.
*/}}
{{- define "kvsynk8s.selectorLabels" -}}
control-plane: controller-manager
app.kubernetes.io/name: kvsynk8s
{{- end -}}

{{/*
ServiceAccount name: the serviceAccount.name override when set, otherwise
<prefix>-controller-manager.
*/}}
{{- define "kvsynk8s.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{- .Values.serviceAccount.name -}}
{{- else -}}
{{- include "kvsynk8s.name" (dict "ctx" . "suffix" "controller-manager") -}}
{{- end -}}
{{- end -}}
