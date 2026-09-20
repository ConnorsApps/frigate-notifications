{{- define "frigate-notify.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Truncated at 63 chars (DNS naming spec); a release name containing the chart name is used as is. */}}
{{- define "frigate-notify.fullname" -}}
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

{{- define "frigate-notify.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "frigate-notify.labels" -}}
helm.sh/chart: {{ include "frigate-notify.chart" . }}
{{ include "frigate-notify.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "frigate-notify.selectorLabels" -}}
app.kubernetes.io/name: {{ include "frigate-notify.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: app
{{- end }}

{{/*
Container env entries for the otel values block. Emits nothing unless an
endpoint is set, so the default install stays free of OTEL_* variables.
*/}}
{{- define "frigate-notify.otelEnv" -}}
{{- $o := .Values.otel }}
{{- if or $o.endpoint $o.traces.endpoint $o.metrics.endpoint }}
{{- $attrs := list }}
{{- if $o.kubernetesAttributes }}
- name: K8S_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: K8S_POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: K8S_NODE_NAME
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
{{- $attrs = concat $attrs (list "k8s.namespace.name=$(K8S_NAMESPACE)" "k8s.pod.name=$(K8S_POD_NAME)" "k8s.node.name=$(K8S_NODE_NAME)") }}
{{- end }}
{{- range $k, $v := $o.resourceAttributes }}
{{- $attrs = append $attrs (printf "%s=%s" $k $v) }}
{{- end }}
{{- if $attrs }}
- name: OTEL_RESOURCE_ATTRIBUTES
  value: {{ join "," $attrs | quote }}
{{- end }}
{{- $headers := list }}
{{- range $k, $v := $o.headers }}
{{- $headers = append $headers (printf "%s=%s" $k $v) }}
{{- end }}
{{- range list
  (list "OTEL_EXPORTER_OTLP_ENDPOINT" $o.endpoint)
  (list "OTEL_EXPORTER_OTLP_HEADERS" (join "," $headers))
  (list "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT" $o.traces.endpoint)
  (list "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL" $o.traces.protocol)
  (list "OTEL_TRACES_SAMPLER" $o.traces.sampler)
  (list "OTEL_TRACES_SAMPLER_ARG" $o.traces.samplerArg)
  (list "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT" $o.metrics.endpoint)
  (list "OTEL_METRIC_EXPORT_INTERVAL" $o.metrics.exportInterval) }}
{{- if index . 1 }}
- name: {{ index . 0 }}
  value: {{ index . 1 | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Labels for a bundled service; takes (dict "root" $ "component" "valkey").
The component label keeps these pods out of the app selectors.
*/}}
{{- define "frigate-notify.componentSelector" -}}
app.kubernetes.io/name: {{ include "frigate-notify.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "frigate-notify.componentLabels" -}}
helm.sh/chart: {{ include "frigate-notify.chart" .root }}
{{ include "frigate-notify.componentSelector" . }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
{{- end }}

{{- define "frigate-notify.valkeyName" -}}{{ include "frigate-notify.fullname" . }}-valkey{{- end }}
{{- define "frigate-notify.postgresName" -}}{{ include "frigate-notify.fullname" . }}-postgres{{- end }}
{{- define "frigate-notify.mongoName" -}}{{ include "frigate-notify.fullname" . }}-mongo{{- end }}
{{- define "frigate-notify.mongoAuthSecret" -}}{{ include "frigate-notify.mongoName" . }}-auth{{- end }}

{{/*
Rejects conflicting values.
*/}}
{{- define "frigate-notify.validate" -}}
{{- $cfg := .Values.config | default dict }}
{{- if and .Values.postgres.enabled .Values.mongo.enabled }}
{{- fail "postgres.enabled and mongo.enabled are mutually exclusive: the app has a single db.url" }}
{{- end }}
{{- if and .Values.valkey.enabled (($cfg.redis | default dict).url) }}
{{- fail "valkey.enabled sets redis.url; remove config.redis.url" }}
{{- end }}
{{- if and (or .Values.postgres.enabled .Values.mongo.enabled) (($cfg.db | default dict).url) }}
{{- fail "postgres.enabled/mongo.enabled set db.url; remove config.db.url" }}
{{- end }}
{{- end }}

{{/*
Wires the app to the bundled services. Passwords live in operator Secrets the
config Secret can't reference, so URLs go via env, which beats the yaml.
*/}}
{{- define "frigate-notify.storeEnv" -}}
{{- if .Values.valkey.enabled }}
- name: REDIS_URL
  value: {{ printf "redis://%s:6379" (include "frigate-notify.valkeyName" .) | quote }}
{{- end }}
{{- if .Values.postgres.enabled }}
- name: DB_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "frigate-notify.postgresName" . }}-app
      key: uri
{{- end }}
{{- if .Values.mongo.enabled }}
- name: DB_URL
  valueFrom:
    secretKeyRef:
      {{- if eq .Values.mongo.mode "operator" }}
      name: {{ include "frigate-notify.mongoName" . }}-admin-{{ .Values.mongo.user }}
      key: connectionString.standard
      {{- else }}
      name: {{ include "frigate-notify.mongoAuthSecret" . }}
      key: url
      {{- end }}
{{- end }}
{{- end }}

{{/*
Deep-merges the user's extraSpec over a rendered spec (maps merge, lists
replace), so any field of a bundled resource can be set without forking.
Takes (dict "base" <spec yaml> "extra" <map>).
*/}}
{{- define "frigate-notify.spec" -}}
{{- toYaml (mustMergeOverwrite (.base | fromYaml) (deepCopy (.extra | default dict))) }}
{{- end }}
