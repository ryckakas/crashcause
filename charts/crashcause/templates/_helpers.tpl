{{/*
Chart name, overridable.
*/}}
{{- define "crashcause.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified resource name.
*/}}
{{- define "crashcause.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "crashcause.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "crashcause.selectorLabels" -}}
app.kubernetes.io/name: {{ include "crashcause.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "crashcause.labels" -}}
helm.sh/chart: {{ include "crashcause.chart" . }}
{{ include "crashcause.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/component: watch
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "crashcause.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "crashcause.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "crashcause.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/*
Port helpers. Both the metrics Service and the health probes need a bare
port number, but the values expose a listen address ("[host]:port") because
that is what the flags take. Split on ":" and take the last segment.
*/}}
{{- define "crashcause.metricsPort" -}}
{{- $addr := .Values.metrics.addr | default ":9090" -}}
{{- $port := splitList ":" $addr | last -}}
{{- if eq $port "" -}}{{- $port = "9090" -}}{{- end -}}
{{- $port -}}
{{- end -}}

{{- define "crashcause.healthPort" -}}
{{- $addr := .Values.healthAddr | default ":8081" -}}
{{- $port := splitList ":" $addr | last -}}
{{- if eq $port "" -}}{{- $port = "8081" -}}{{- end -}}
{{- $port -}}
{{- end -}}

{{/*
The controller's full argument vector, rendered as a YAML list.

Flags whose value comes from values.yaml are passed explicitly rather than
relied on as binary defaults, so that `kubectl describe pod` shows exactly
the configuration the chart was rendered with. --collect-logs in particular
is ALWAYS explicit: it is the flag half of the severable log-collection
privilege, and its state must be readable off the Deployment without
cross-referencing the chart version's defaults.
*/}}
{{- define "crashcause.args" -}}
{{- $args := list "watch" -}}
{{- $args = append $args (printf "--log-level=%s" .Values.logLevel) -}}
{{- if .Values.watch.namespaces -}}
{{- $args = append $args (printf "--namespaces=%s" (join "," .Values.watch.namespaces)) -}}
{{- end -}}
{{- if .Values.watch.selector -}}
{{- $args = append $args (printf "--selector=%s" .Values.watch.selector) -}}
{{- end -}}
{{- $args = append $args (printf "--reemit-interval=%v" .Values.watch.reemitInterval) -}}
{{- $args = append $args (printf "--dedup-ttl=%v" .Values.watch.dedupTTL) -}}
{{- $args = append $args (printf "--previous-lines=%v" .Values.watch.previousLines) -}}
{{- $args = append $args (printf "--init-stuck-threshold=%v" .Values.watch.initStuckThreshold) -}}
{{- $args = append $args (printf "--collect-logs=%v" .Values.logCollection.enabled) -}}
{{- if .Values.logCollection.enabled -}}
{{- $args = append $args (printf "--log-rate-limit=%v" .Values.logCollection.rateLimitPerMinute) -}}
{{- if .Values.logCollection.namespaces -}}
{{- $args = append $args (printf "--log-namespaces=%s" (join "," .Values.logCollection.namespaces)) -}}
{{- end -}}
{{- end -}}
{{- if .Values.metrics.enabled -}}
{{- $args = append $args (printf "--metrics-addr=%s" .Values.metrics.addr) -}}
{{- end -}}
{{- if .Values.loki.url -}}
{{- $args = append $args (printf "--loki-url=%s" .Values.loki.url) -}}
{{- end -}}
{{- $args = append $args (printf "--health-addr=%s" .Values.healthAddr) -}}
{{- if .Values.leaderElection.enabled -}}
{{- $args = append $args "--leader-elect" -}}
{{- $args = append $args (printf "--leader-election-id=%s" (include "crashcause.fullname" .)) -}}
{{- end -}}
{{- if .Values.ai.enabled -}}
{{- $args = append $args "--ai" -}}
{{- $args = append $args (printf "--ai-provider=%s" .Values.ai.provider) -}}
{{- if .Values.ai.url -}}
{{- $args = append $args (printf "--ai-url=%s" .Values.ai.url) -}}
{{- end -}}
{{- $args = append $args (printf "--ai-redact=%v" .Values.ai.redact) -}}
{{- $args = append $args (printf "--ai-redact-ips=%v" .Values.ai.redactIPs) -}}
{{- if .Values.ai.namespaces -}}
{{- $args = append $args (printf "--ai-namespaces=%s" (join "," .Values.ai.namespaces)) -}}
{{- end -}}
{{- end -}}
{{- range .Values.extraArgs -}}
{{- $args = append $args . -}}
{{- end -}}
{{- toYaml $args -}}
{{- end -}}
