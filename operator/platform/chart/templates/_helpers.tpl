{{/*
Shared conventions for the zaentrum platform chart. Every service template uses
these so image/issuer/hostAliases/pull-secrets/labels stay consistent.
*/}}

{{/* z.issuer — the OIDC issuer URL (explicit override, else derived). */}}
{{- define "z.issuer" -}}
{{- if .Values.identity.issuer -}}
{{- .Values.identity.issuer -}}
{{- else -}}
{{- .Values.identity.issuerScheme }}://{{ .Values.global.hostname }}/auth/realms/zaentrum
{{- end -}}
{{- end -}}

{{/* z.kcHostname — KC_HOSTNAME for the bundled Keycloak (scheme+host+/auth). */}}
{{- define "z.kcHostname" -}}
{{- .Values.identity.issuerScheme }}://{{ .Values.global.hostname }}/auth
{{- end -}}

{{/* z.partOf — the app.kubernetes.io/part-of label value. */}}
{{- define "z.partOf" -}}{{ .Values.global.partOf }}{{- end -}}

{{/*
z.replicas — per-service replica count from .Values.services.<name>.replicas,
falling back to a default. (Avoids Sprig `dig`, which rejects the typed
chartutil.Values.) Use: replicas: {{ include "z.replicas" (dict "root" $ "name" "chino-api" "def" 1) }}
*/}}
{{- define "z.replicas" -}}
{{- $r := .def -}}
{{- $svcs := .root.Values.services -}}
{{- if $svcs -}}
{{- $svc := index $svcs .name -}}
{{- if $svc -}}{{- $r = ($svc.replicas | default .def) -}}{{- end -}}
{{- end -}}
{{- $r -}}
{{- end -}}

{{/*
z.hostAliases — split-horizon hostAliases so an OIDC validator resolves the
public issuer host to the router node. Emits nothing when unset. Place under
spec.template.spec:  {{- include "z.hostAliases" . | nindent 6 }}
*/}}
{{- define "z.hostAliases" -}}
{{- if .Values.network.issuerHostAliasIP }}
hostAliases:
  - ip: "{{ .Values.network.issuerHostAliasIP }}"
    hostnames: ["{{ .Values.global.hostname }}"]
{{- end }}
{{- end -}}

{{/*
z.imagePullSecrets — the pull-secrets block, or nothing. Place under
spec.template.spec:  {{- include "z.imagePullSecrets" . | nindent 6 }}
*/}}
{{- define "z.imagePullSecrets" -}}
{{- with .Values.global.imagePullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}

{{/* z.kafkaBrokers — bootstrap servers: the bundled broker or the shared cluster. */}}
{{- define "z.kafkaBrokers" -}}
{{- if eq .Values.eventStreaming.mode "external" -}}{{ required "eventStreaming.bootstrap is required in external mode" .Values.eventStreaming.bootstrap }}{{- else -}}kafka:9092{{- end -}}
{{- end -}}

{{/* z.topicPrefix — per-tenant Kafka topic namespace. */}}
{{- define "z.topicPrefix" -}}{{ .Values.eventStreaming.topicPrefix | default "stube." }}{{- end -}}

{{/* z.kafkaEnv — the common Kafka env block (brokers + prefix + cert dir). */}}
{{- define "z.kafkaEnv" -}}
- name: KAFKA_BROKERS
  value: {{ include "z.kafkaBrokers" . }}
- name: KAFKA_TOPIC_PREFIX
  value: {{ include "z.topicPrefix" . | quote }}
{{- if and (eq .Values.eventStreaming.mode "external") .Values.eventStreaming.certSecret }}
- name: KAFKA_CERT_DIR
  value: /etc/kafka-cert
{{- end }}
{{- end -}}

{{/* z.kafkaCertMount / z.kafkaCertVolume — mTLS material for the shared cluster.
     Emit bare list items (no leading newline); wrap call sites in `with` so
     bundled mode renders nothing (not even whitespace). */}}
{{- define "z.kafkaCertMount" -}}
{{- if and (eq .Values.eventStreaming.mode "external") .Values.eventStreaming.certSecret -}}
- { name: kafka-cert, mountPath: /etc/kafka-cert, readOnly: true }
{{- end -}}
{{- end -}}
{{- define "z.kafkaCertVolume" -}}
{{- if and (eq .Values.eventStreaming.mode "external") .Values.eventStreaming.certSecret -}}
- name: kafka-cert
  secret: { secretName: {{ .Values.eventStreaming.certSecret | quote }} }
{{- end -}}
{{- end -}}

{{/* z.workerOIDCExternalEnv — pipeline-worker client-credentials against an
     EXTERNAL realm: token endpoint derived from the issuer; client id+secret
     from the CI-provided zaentrum-worker-oidc Secret (mirrors the katalog-oidc
     pattern prod stube already runs by hand). */}}
{{- define "z.workerOIDCExternalEnv" -}}
- name: OIDC_TOKEN_URL
  value: {{ include "z.issuer" . }}/protocol/openid-connect/token
- name: OIDC_CLIENT_ID
  valueFrom:
    secretKeyRef:
      key: client-id
      name: zaentrum-worker-oidc
- name: OIDC_CLIENT_SECRET
  valueFrom:
    secretKeyRef:
      key: client-secret
      name: zaentrum-worker-oidc
{{- end -}}

{{/* z.pgHost — host:port of the platform database (bundled or shared). */}}
{{- define "z.pgHost" -}}
{{- if eq .Values.databases.mode "external" -}}{{ required "databases.external.host is required in external mode" .Values.databases.external.host }}:{{ .Values.databases.external.port | default 5432 }}{{- else -}}postgres:5432{{- end -}}
{{- end -}}

{{/* z.pgSSLMode — disable for the bundled plaintext postgres, configurable external. */}}
{{- define "z.pgSSLMode" -}}
{{- if eq .Values.databases.mode "external" -}}{{ .Values.databases.external.sslmode | default "require" }}{{- else -}}disable{{- end -}}
{{- end -}}

{{/* z.pgURL — a full DSN for one database: pass (dict "root" $ "db" <name>).
     Credentials stay $(DB_USER)/$(DB_PASSWORD) env expansion at the consumer. */}}
{{- define "z.pgURL" -}}
postgres://$(DB_USER):$(DB_PASSWORD)@{{ include "z.pgHost" .root }}/{{ .db }}?sslmode={{ include "z.pgSSLMode" .root }}
{{- end -}}

{{/* z.chinoHost — the host serving the chino SPA (subdomains mode), else the
     global host. z.chinoBase — the SPA base path on that host. */}}
{{- define "z.chinoHost" -}}
{{- if and (eq .Values.routing.mode "subdomains") .Values.routing.hosts.chino -}}{{ .Values.routing.hosts.chino }}{{- else -}}{{ .Values.global.hostname }}{{- end -}}
{{- end -}}
{{- define "z.chinoBase" -}}
{{- if and (eq .Values.routing.mode "subdomains") .Values.routing.hosts.chino -}}/{{- else -}}/chino{{- end -}}
{{- end -}}
