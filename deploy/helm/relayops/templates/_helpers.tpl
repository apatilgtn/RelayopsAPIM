{{- define "relayops.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "relayops.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "relayops.labels" -}}
app.kubernetes.io/name: {{ include "relayops.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "relayops.selectorLabels" -}}
app.kubernetes.io/name: {{ include "relayops.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "relayops.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "relayops.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "relayops.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "relayops.secretName" -}}
{{ include "relayops.fullname" . }}
{{- end -}}

{{/* Admin token: explicit value, else reuse the previously generated one, else generate. */}}
{{- define "relayops.adminToken" -}}
{{- if .Values.adminToken.value -}}
{{- .Values.adminToken.value -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "relayops.secretName" .) -}}
{{- if and $existing (index $existing.data "admin-token") -}}
{{- index $existing.data "admin-token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Whether the chart runs separate control-plane and gateway-only pods. */}}
{{- define "relayops.split" -}}
{{- if eq .Values.mode "split" }}true{{ end -}}
{{- end -}}

{{/* Node API token for split mode: explicit value, else reuse the generated one, else generate. */}}
{{- define "relayops.dataplaneToken" -}}
{{- if .Values.dataplane.token.value -}}
{{- .Values.dataplane.token.value -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "relayops.secretName" .) -}}
{{- if and $existing (index $existing.data "dataplane-token") -}}
{{- index $existing.data "dataplane-token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Test Studio runner secret, shared by control-plane and gateway pods in split mode. */}}
{{- define "relayops.runnerSecret" -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "relayops.secretName" .) -}}
{{- if and $existing (index $existing.data "runner-secret") -}}
{{- index $existing.data "runner-secret" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}

{{/* Control plane URL gateway-only pods pull configuration from. */}}
{{- define "relayops.controlPlaneURL" -}}
{{- default (printf "http://%s-admin:%v" (include "relayops.fullname" .) .Values.service.adminPort) .Values.dataplane.controlPlaneURL -}}
{{- end -}}

{{/*
Pod template shared by the stable, canary and control-plane deployments.
Call with (dict "root" $ "role" "gateway"|"canary"|"control-plane" "mode" "combined"|"gateway"|"control-plane"
           "group" <node group> "canary" <bool> "resources" <map>)
mode "combined" runs gateway and control plane in one process (RELAYOPS_ROLE=all).
*/}}
{{- define "relayops.podTemplate" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
{{- $mode := .mode | default "combined" -}}
metadata:
  labels:
    {{- include "relayops.selectorLabels" $root | nindent 4 }}
    app.kubernetes.io/component: {{ .role }}
    {{- if ne $mode "control-plane" }}
    relayops.io/serves-proxy: "true"
    {{- end }}
  annotations:
    checksum/secret: {{ include (print $root.Template.BasePath "/secret.yaml") $root | sha256sum }}
    {{- with $v.gateway.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  serviceAccountName: {{ include "relayops.serviceAccountName" $root }}
  {{- if and (eq $mode "control-plane") $v.controlPlane.leaderElection }}
  automountServiceAccountToken: true # Lease API access for leader election
  {{- end }}
  {{- with $v.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  securityContext:
    {{- toYaml $v.podSecurityContext | nindent 4 }}
  terminationGracePeriodSeconds: 30
  containers:
    - name: relayops
      image: {{ include "relayops.image" $root }}
      imagePullPolicy: {{ $v.image.pullPolicy }}
      workingDir: /home/nonroot
      securityContext:
        {{- toYaml $v.securityContext | nindent 8 }}
      ports:
        {{- if ne $mode "control-plane" }}
        - {name: proxy, containerPort: 8080, protocol: TCP}
        {{- end }}
        {{- if ne $mode "gateway" }}
        - {name: admin, containerPort: 9090, protocol: TCP}
        {{- else }}
        - {name: status, containerPort: 9091, protocol: TCP}
        {{- end }}
      env:
        - name: RELAYOPS_NODE_ID
          valueFrom: {fieldRef: {fieldPath: metadata.name}}
        - {name: RELAYOPS_NODE_GROUP, value: {{ .group | quote }}}
        - {name: RELAYOPS_CANARY, value: {{ .canary | quote }}}
        - {name: RELAYOPS_PROXY_ADDR, value: ":8080"}
        - {name: RELAYOPS_ADMIN_ADDR, value: ":9090"}
        - {name: RELAYOPS_LOG_RETENTION_HOURS, value: {{ $v.logRetentionHours | quote }}}
        - {name: RELAYOPS_RESYNC_SECONDS, value: {{ $v.resyncSeconds | quote }}}
        - {name: RELAYOPS_LOG_SPOOL_MB, value: {{ $v.logSpoolMB | quote }}}
        - {name: RELAYOPS_LOG_SAMPLE_RATE, value: {{ $v.logSampleRate | quote }}}
        {{- with $v.logExport }}
        - {name: RELAYOPS_LOG_EXPORT, value: {{ . | quote }}}
        {{- end }}
        {{- if ne $mode "combined" }}
        - {name: RELAYOPS_ROLE, value: {{ $mode | quote }}}
        - name: RELAYOPS_DATAPLANE_TOKEN
          valueFrom:
            secretKeyRef:
              {{- if $v.dataplane.token.existingSecret }}
              name: {{ $v.dataplane.token.existingSecret }}
              key: {{ $v.dataplane.token.existingSecretKey }}
              {{- else }}
              name: {{ include "relayops.secretName" $root }}
              key: dataplane-token
              {{- end }}
        - name: RELAYOPS_RUNNER_SECRET
          valueFrom:
            secretKeyRef:
              name: {{ include "relayops.secretName" $root }}
              key: runner-secret
        {{- end }}
        {{- if and (eq $mode "control-plane") $v.controlPlane.leaderElection }}
        - {name: RELAYOPS_LEADER_ELECTION, value: "kubernetes"}
        {{- end }}
        {{- if eq $mode "control-plane" }}
        - {name: RELAYOPS_GATEWAY_URL, value: {{ printf "http://%s-proxy:%v" (include "relayops.fullname" $root) $v.service.proxyPort | quote }}}
        {{- if $v.dataplane.signingKey.existingSecret }}
        - name: RELAYOPS_DATAPLANE_SIGNING_KEY
          valueFrom:
            secretKeyRef:
              name: {{ $v.dataplane.signingKey.existingSecret }}
              key: {{ $v.dataplane.signingKey.existingSecretKey }}
        {{- end }}
        {{- end }}
        {{- if and (eq $mode "gateway") $v.dataplane.verifyKeys }}
        - {name: RELAYOPS_DATAPLANE_VERIFY_KEYS, value: {{ $v.dataplane.verifyKeys | quote }}}
        {{- end }}
        {{- if eq $mode "gateway" }}
        {{- /* Gateway-only pods get no database DSN, admin token or SSO secret. */}}
        - {name: RELAYOPS_CONTROL_PLANE_URL, value: {{ include "relayops.controlPlaneURL" $root | quote }}}
        - {name: RELAYOPS_DATAPLANE_ALLOW_HTTP, value: {{ $v.dataplane.allowHTTP | quote }}}
        - {name: RELAYOPS_STATUS_ADDR, value: ":9091"}
        {{- else }}
        - name: RELAYOPS_DATABASE_URL
          valueFrom:
            secretKeyRef:
              {{- if $v.database.existingSecret }}
              name: {{ $v.database.existingSecret }}
              key: {{ $v.database.existingSecretKey }}
              {{- else }}
              name: {{ include "relayops.secretName" $root }}
              key: database-url
              {{- end }}
        - name: RELAYOPS_ADMIN_TOKEN
          valueFrom:
            secretKeyRef:
              {{- if $v.adminToken.existingSecret }}
              name: {{ $v.adminToken.existingSecret }}
              key: {{ $v.adminToken.existingSecretKey }}
              {{- else }}
              name: {{ include "relayops.secretName" $root }}
              key: admin-token
              {{- end }}
        {{- if or $v.ssoSecret.value $v.ssoSecret.existingSecret }}
        - name: RELAYOPS_SSO_SECRET
          valueFrom:
            secretKeyRef:
              {{- if $v.ssoSecret.existingSecret }}
              name: {{ $v.ssoSecret.existingSecret }}
              key: {{ $v.ssoSecret.existingSecretKey }}
              {{- else }}
              name: {{ include "relayops.secretName" $root }}
              key: sso-secret
              {{- end }}
        {{- end }}
        {{- end }}
        {{- if $v.redis.url }}
        - {name: RELAYOPS_REDIS_URL, value: {{ $v.redis.url | quote }}}
        {{- else }}
        - {name: RELAYOPS_REDIS_URL, value: "disabled"}
        {{- end }}
        {{- range $k, $_ := $v.apiSecrets }}
        - name: RELAYOPS_SECRET_{{ $k }}
          valueFrom:
            secretKeyRef:
              name: {{ include "relayops.secretName" $root }}
              key: api-secret-{{ $k | lower | replace "_" "-" }}
        {{- end }}
        {{- if $v.tracing.endpoint }}
        - {name: OTEL_EXPORTER_OTLP_ENDPOINT, value: {{ $v.tracing.endpoint | quote }}}
        - {name: OTEL_SERVICE_NAME, value: {{ $v.tracing.serviceName | quote }}}
        - {name: OTEL_TRACES_SAMPLER_ARG, value: {{ $v.tracing.sampleRatio | quote }}}
        {{- if $v.tracing.headers }}
        - {name: OTEL_EXPORTER_OTLP_HEADERS, value: {{ $v.tracing.headers | quote }}}
        {{- end }}
        {{- end }}
        {{- with $v.extraEnv }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with $v.extraEnvFrom }}
      envFrom:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- if eq $mode "control-plane" }}
      startupProbe:
        httpGet: {path: /healthz, port: admin}
        periodSeconds: 2
        failureThreshold: 30
      livenessProbe:
        httpGet: {path: /healthz, port: admin}
        periodSeconds: 10
        failureThreshold: 3
      {{- else }}
      startupProbe:
        httpGet: {path: /__relayops/health, port: proxy}
        periodSeconds: 2
        failureThreshold: 30
      livenessProbe:
        httpGet: {path: /__relayops/health, port: proxy}
        periodSeconds: 10
        failureThreshold: 3
      {{- end }}
      readinessProbe:
        {{- if eq $mode "gateway" }}
        httpGet: {path: /readyz, port: status}
        {{- else }}
        httpGet: {path: /healthz, port: admin}
        {{- end }}
        periodSeconds: 5
        failureThreshold: 2
      resources:
        {{- toYaml .resources | nindent 8 }}
      volumeMounts:
        - {name: config-cache, mountPath: /home/nonroot/data}
        - {name: tmp, mountPath: /tmp}
  volumes:
    {{- if not .persistent }}
    - name: config-cache
      emptyDir:
        sizeLimit: {{ printf "%dMi" (add (int $v.logSpoolMB) 64) }}
    {{- end }}
    - {name: tmp, emptyDir: {}}
  {{- with $v.gateway.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.gateway.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.gateway.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.gateway.topologySpreadConstraints }}
  topologySpreadConstraints:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}
