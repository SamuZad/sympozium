{{/*
Expand the name of the chart.
*/}}
{{- define "sympozium.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "sympozium.fullname" -}}
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
Chart label helper.
*/}}
{{- define "sympozium.labels" -}}
helm.sh/chart: {{ include "sympozium.chart" . }}
{{ include "sympozium.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: sympozium
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "sympozium.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sympozium.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Chart name and version.
*/}}
{{- define "sympozium.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Image tag helper — defaults to Chart.AppVersion.
*/}}
{{- define "sympozium.imageTag" -}}
{{- .Values.image.tag | default (printf "v%s" .Chart.AppVersion) }}
{{- end }}

{{/*
Controller image.
*/}}
{{- define "sympozium.controllerImage" -}}
{{- $repo := .Values.controller.image.repository | default (printf "%s/controller" .Values.image.registry) }}
{{- $tag := .Values.controller.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
API server image.
*/}}
{{- define "sympozium.apiserverImage" -}}
{{- $repo := .Values.apiserver.image.repository | default (printf "%s/apiserver" .Values.image.registry) }}
{{- $tag := .Values.apiserver.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
Webhook image.
*/}}
{{- define "sympozium.webhookImage" -}}
{{- $repo := .Values.webhook.image.repository | default (printf "%s/webhook" .Values.image.registry) }}
{{- $tag := .Values.webhook.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
Web proxy image.
*/}}
{{- define "sympozium.webProxyImage" -}}
{{- $repo := .Values.webProxy.image.repository | default (printf "%s/web-proxy" .Values.image.registry) }}
{{- $tag := .Values.webProxy.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
Node probe image.
*/}}
{{- define "sympozium.nodeProbeImage" -}}
{{- $repo := .Values.nodeProbe.image.repository | default (printf "%s/node-probe" .Values.image.registry) }}
{{- $tag := .Values.nodeProbe.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
llmfit daemon image.
*/}}
{{- define "sympozium.llmfitDaemonImage" -}}
{{- $repo := .Values.llmfit.daemonset.image.repository | default (printf "%s/llmfit-daemon" .Values.image.registry) }}
{{- $tag := .Values.llmfit.daemonset.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
NATS URL — internal or external.
*/}}
{{- define "sympozium.natsUrl" -}}
{{- if .Values.nats.enabled }}
{{- printf "nats://nats.%s.svc:4222" .Values.namespace }}
{{- else }}
{{- .Values.nats.externalUrl }}
{{- end }}
{{- end }}

{{/*
Namespace helper.
*/}}
{{- define "sympozium.namespace" -}}
{{- .Values.namespace | default "sympozium-system" }}
{{- end }}

{{/*
OTel headers: convert map to comma-separated "key=value" pairs.
*/}}
{{- define "sympozium.otelHeaders" -}}
{{- $pairs := list -}}
{{- range $k, $v := .Values.observability.headers -}}
{{- $pairs = append $pairs (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end }}

{{/*
OTel resource attributes: convert map to comma-separated "key=value" pairs.
*/}}
{{- define "sympozium.otelResourceAttrs" -}}
{{- $pairs := list -}}
{{- range $k, $v := .Values.observability.resourceAttributes -}}
{{- $pairs = append $pairs (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end }}

{{/*
Memory server image.
*/}}
{{- define "sympozium.memoryServerImage" -}}
{{- $repo := .Values.memory.server.image.repository | default (printf "%s/memory-server" .Values.image.registry) }}
{{- $tag := .Values.memory.server.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
Memory server in-cluster URL — used by the controller to set
MEMORY_SERVER_URL on agent pods.
*/}}
{{- define "sympozium.memoryServerURL" -}}
{{- printf "http://%s-memory-server.%s.svc:%d" (include "sympozium.fullname" .) (include "sympozium.namespace" .) (int .Values.memory.server.service.port) }}
{{- end }}

{{/*
Artifact server image.
*/}}
{{- define "sympozium.artifactServerImage" -}}
{{- $repo := .Values.artifact.image.repository | default (printf "%s/artifact-server" .Values.image.registry) }}
{{- $tag := .Values.artifact.image.tag | default (include "sympozium.imageTag" .) }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}

{{/*
Artifact server in-cluster URL — used by the controller to set
ARTIFACT_SERVER_URL on agent and channel pods.
*/}}
{{- define "sympozium.artifactServerURL" -}}
{{- printf "http://%s-artifact-server.%s.svc:%d" (include "sympozium.fullname" .) (include "sympozium.namespace" .) (int .Values.artifact.service.port) }}
{{- end }}

{{/*
Bundled Postgres connection string. Only used when memory.postgres.enabled
AND no database.url / database.urlSecret is set.
*/}}
{{- define "sympozium.bundledPostgresURL" -}}
{{- $host := printf "%s-postgres.%s.svc" (include "sympozium.fullname" .) (include "sympozium.namespace" .) -}}
{{- $port := int .Values.memory.postgres.service.port -}}
{{- $db := .Values.memory.postgres.auth.database -}}
{{- $user := .Values.memory.postgres.auth.username -}}
{{- printf "postgres://%s:$(POSTGRES_PASSWORD)@%s:%d/%s?sslmode=disable" $user $host $port $db -}}
{{- end }}

{{/*
Name of the Secret that holds the bundled Postgres password.
*/}}
{{- define "sympozium.bundledPostgresSecret" -}}
{{- printf "%s-postgres" (include "sympozium.fullname" .) -}}
{{- end }}

{{/*
Name of the Secret that holds the embedding API key (when chart-managed).
*/}}
{{- define "sympozium.memoryEmbeddingSecret" -}}
{{- printf "%s-memory-embedding" (include "sympozium.fullname" .) -}}
{{- end }}

{{/*
Memory server env block — shared between the migration Job and the serve
Deployment so they always see the same connection + embedding config.
Renders the `env:` list contents (no leading `env:` key).
*/}}
{{- define "sympozium.memoryServerEnv" -}}
{{- /* DATABASE_URL: secretRef > raw url > bundled postgres */ -}}
{{- if .Values.memory.database.urlSecret }}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.memory.database.urlSecret | quote }}
      key: DATABASE_URL
{{- else if .Values.memory.database.url }}
- name: DATABASE_URL
  value: {{ tpl .Values.memory.database.url . | quote }}
{{- else if .Values.memory.postgres.enabled }}
{{- $secretName := .Values.memory.postgres.auth.existingSecret | default (include "sympozium.bundledPostgresSecret" .) }}
- name: POSTGRES_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $secretName | quote }}
      key: password
- name: DATABASE_URL
  value: {{ include "sympozium.bundledPostgresURL" . | quote }}
{{- else }}
{{- fail "memory.enabled is true but no database is configured. Either point at an external Postgres (memory.database.url or memory.database.urlSecret), OR enable the bundled single-node Postgres (memory.postgres.enabled=true, suitable for dev/kind only), OR disable memory entirely (memory.enabled=false)." }}
{{- end }}
- name: MEMORY_DB_AUTH
  value: {{ .Values.memory.database.auth.mode | quote }}
{{- if .Values.memory.database.auth.awsRegion }}
- name: AWS_REGION
  value: {{ .Values.memory.database.auth.awsRegion | quote }}
{{- end }}
- name: MEMORY_LISTEN
  value: {{ .Values.memory.server.listen | quote }}
- name: MEMORY_NAMESPACE
  value: {{ include "sympozium.namespace" . | quote }}
- name: MEMORY_EMBEDDING_PROVIDER
  value: {{ .Values.memory.embedding.provider | quote }}
- name: MEMORY_EMBEDDING_MODEL
  value: {{ .Values.memory.embedding.model | quote }}
- name: MEMORY_EMBEDDING_DIM
  value: {{ .Values.memory.embedding.dimension | quote }}
{{- if .Values.memory.embedding.baseURL }}
- name: MEMORY_EMBEDDING_BASE_URL
  value: {{ .Values.memory.embedding.baseURL | quote }}
{{- end }}
{{- if or .Values.memory.embedding.apiKey .Values.memory.embedding.apiKeySecretRef }}
- name: MEMORY_EMBEDDING_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.memory.embedding.apiKeySecretRef | default (include "sympozium.memoryEmbeddingSecret" .) | quote }}
      key: EMBEDDING_API_KEY
{{- end }}
- name: MEMORY_DEFAULT_TTL_DAYS
  value: {{ .Values.memory.server.defaultTTLDays | quote }}
- name: MEMORY_TOKEN_CACHE_TTL
  value: {{ .Values.memory.server.tokenCache.ttl | quote }}
- name: MEMORY_TOKEN_CACHE_SIZE
  value: {{ .Values.memory.server.tokenCache.size | quote }}
- name: MEMORY_MEMBERSHIP_CACHE_TTL
  value: {{ .Values.memory.server.membershipCache.ttl | quote }}
- name: MEMORY_MEMBERSHIP_CACHE_SIZE
  value: {{ .Values.memory.server.membershipCache.size | quote }}
{{- $admins := .Values.memory.server.adminServiceAccounts | default list }}
{{- /* Always include the controller + apiserver SAs as admins. */ -}}
{{- $admins = append $admins (printf "%s/sympozium-controller-manager" (include "sympozium.namespace" .)) }}
{{- $admins = append $admins (printf "%s/sympozium-apiserver" (include "sympozium.namespace" .)) }}
- name: MEMORY_ADMIN_SAS
  value: {{ join "," $admins | quote }}
{{- end }}
