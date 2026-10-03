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

{{/* z.publicURL — the origin users reach the platform at: https where the edge
     terminates TLS (OpenShift Routes always do; issuerScheme https says a proxy
     in front does), else http. The verification Job checks the platform from
     there, outside-in, the way a user meets it. */}}
{{- define "z.publicURL" -}}
{{- if or (eq .Values.identity.issuerScheme "https") .Values.routing.provisionRoutes -}}https{{- else -}}http{{- end -}}://{{ .Values.global.hostname }}
{{- end -}}

{{/*
z.secretValue — a value for one key of a Secret the chart renders for a plain
Helm install: what the cluster's Secret already holds (lookup, so an upgrade
never rotates it), else .value, else .length (default 32) random alphanumerics.
Use: {{ include "z.secretValue" (dict "root" . "secret" "zaentrum-db" "key" "password") }}
*/}}
{{- define "z.secretValue" -}}
{{- $have := "" -}}
{{- $live := lookup "v1" "Secret" .root.Release.Namespace .secret -}}
{{- if and $live $live.data -}}
{{- with index $live.data .key -}}{{- $have = b64dec . -}}{{- end -}}
{{- end -}}
{{- if $have -}}{{ $have }}{{- else if .value -}}{{ .value }}{{- else -}}{{ randAlphaNum (.length | default 32) }}{{- end -}}
{{- end -}}

{{/* z.katalogBase / z.katalogManageBase — where the two katalog consoles are
     mounted (their BASE_PATH), which is also where they sign in from. */}}
{{- define "z.katalogBase" -}}/katalog/{{- end -}}
{{- define "z.katalogManageBase" -}}/katalog-manage/{{- end -}}

{{/*
z.mainOrigins — every origin the platform's own host answers on, as a JSON
list: the public URL, and plain http beside it where OpenShift Routes serve the
host (they allow http: insecureEdgeTerminationPolicy Allow, routes.yaml).
*/}}
{{- define "z.mainOrigins" -}}
{{- $origins := list (include "z.publicURL" .) -}}
{{- $http := printf "http://%s" .Values.global.hostname -}}
{{- if and .Values.routing.provisionRoutes (not (has $http $origins)) -}}
{{- $origins = append $origins $http -}}
{{- end -}}
{{- toJson $origins -}}
{{- end -}}

{{/*
z.realmClients — what the bundled realm's clients allow a sign-in to return to,
as JSON: for each client, its redirect URIs, web origins and post-logout
redirect URIs, exactly those of the platform's own origins and paths, and
nothing else. The realm import (keycloak-realm.yaml) writes them into a new
realm; the realm Job (realm.yaml) writes them into one that exists.

  zaentrum-web  the portal at /portal/ and the katalog consoles, which sign in
                as the portal client; each returns to <origin><base>auth/callback
                and, signed out, to <origin><base>
  chino-web     returns to the site root's /auth/callback (it builds it so,
                whatever its base, and the verification signs in the same
                way), on the main host and on the chino host of subdomains
                routing; signed out, to the bare origin
  chino-mobile  the published phone and tablet apps' custom scheme
  chino-tv      the device grant: no redirect at all
  zae           the command line's loopback redirects
"+" as a post-logout value means the client's redirect URIs — what Keycloak
gives a client that names none, and so what an import leaves.
*/}}
{{- define "z.realmClients" -}}
{{- $main := include "z.mainOrigins" . | fromJsonArray -}}
{{- $web := $main -}}
{{- if and (eq .Values.routing.mode "subdomains") .Values.routing.hosts.chino -}}
{{- $web = append $web (printf "https://%s" .Values.routing.hosts.chino) -}}
{{- end -}}
{{- $chinoBack := list -}}
{{- range $web -}}{{- $chinoBack = append $chinoBack (printf "%s/auth/callback" .) -}}{{- end -}}
{{- $portalBack := list -}}
{{- $portalOut := list -}}
{{- range $origin := $main -}}
{{- range $base := list "/portal/" (include "z.katalogBase" $) (include "z.katalogManageBase" $) -}}
{{- $portalBack = append $portalBack (printf "%s%sauth/callback" $origin $base) -}}
{{- $portalOut = append $portalOut (printf "%s%s" $origin $base) -}}
{{- end -}}
{{- end -}}
{{- dict
    "zaentrum-web" (dict "redirectUris" $portalBack "webOrigins" $main "attributes" (dict "post.logout.redirect.uris" (join "##" $portalOut)))
    "chino-web" (dict "redirectUris" $chinoBack "webOrigins" $web "attributes" (dict "post.logout.redirect.uris" (join "##" $web)))
    "chino-mobile" (dict "redirectUris" (list "cloud.nalet.chino:/oauth/callback") "webOrigins" (list) "attributes" (dict "post.logout.redirect.uris" "+"))
    "chino-tv" (dict "redirectUris" (list) "webOrigins" (list) "attributes" (dict "post.logout.redirect.uris" "+"))
    "zae" (dict "redirectUris" (list "http://127.0.0.1/*" "http://localhost/*") "webOrigins" (list) "attributes" (dict "post.logout.redirect.uris" "+"))
  | toJson -}}
{{- end -}}

{{/*
z.adminConsoleURL — where the bundled Keycloak's admin console signs in and is
served: the public host when identity.exposeAdminConsole, else only the
port-forward `kubectl port-forward svc/keycloak 8080:80` makes, so that neither
the console nor its sign-in needs a public route. Empty: the public host.
*/}}
{{- define "z.adminConsoleURL" -}}
{{- if not .Values.identity.exposeAdminConsole -}}http://localhost:8080/auth{{- end -}}
{{- end -}}

{{/* z.postgresClaim — the claim the bundled Postgres keeps its data on. */}}
{{- define "z.postgresClaim" -}}
{{- .Values.storage.postgres.claimName | default "postgres-data" -}}
{{- end -}}

{{/*
z.postgresVolume — where the running Postgres's data is this render: "emptyDir"
or a claim's name. storage.postgres.current when set (the operator always sets
it); else what the running Postgres Deployment mounts (lookup, under helm
install/upgrade), so that no upgrade moves a database by itself; else — a new
install, or `helm template` — the claim.
*/}}
{{- define "z.postgresVolume" -}}
{{- $volume := .Values.storage.postgres.current -}}
{{- if not $volume -}}
{{- $volume = include "z.postgresClaim" . -}}
{{- $live := lookup "apps/v1" "Deployment" .Release.Namespace "postgres" -}}
{{- range (dig "spec" "template" "spec" "volumes" (list) $live) -}}
{{- if eq .name "data" -}}
{{- if hasKey . "emptyDir" -}}{{- $volume = "emptyDir" -}}
{{- else if .persistentVolumeClaim -}}{{- $volume = .persistentVolumeClaim.claimName -}}{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $volume -}}
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
z.podSecurity — the pod securityContext of every platform pod: never root, the
runtime's default seccomp profile. On OpenShift the SCC gives each pod a user
from the namespace's range, so the chart names none (a fixed one would fall
outside it). Anywhere else the kubelet must see a numeric user to verify
runAsNonRoot, and an image that names its user (distroless "nonroot") or none
(root, as postgres:16-alpine's init steps) would not start — so the pod runs as
65532, distroless's nonroot user, as OpenShift runs any image as an arbitrary
user. OpenShift is told by its security API in .Capabilities: plain Helm sees
the cluster's, and the operator passes what it discovered.
Place under spec.template.spec:  {{- include "z.podSecurity" . | nindent 6 }}
*/}}
{{- define "z.podSecurity" -}}
securityContext:
  runAsNonRoot: true
{{- if not (.Capabilities.APIVersions.Has "security.openshift.io/v1") }}
  runAsUser: 65532
{{- end }}
  seccompProfile:
    type: RuntimeDefault
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

{{/* z.kafkaCertSecret — the secret holding the shared cluster's mTLS material
     (user.crt/user.key/ca.crt), or empty: the bundled broker is plaintext. */}}
{{- define "z.kafkaCertSecret" -}}
{{- if eq .Values.eventStreaming.mode "external" -}}{{ .Values.eventStreaming.certSecret }}{{- end -}}
{{- end -}}

{{/* z.kafkaEnv — the common Kafka env block (brokers + prefix + cert dir). */}}
{{- define "z.kafkaEnv" -}}
- name: KAFKA_BROKERS
  value: {{ include "z.kafkaBrokers" . }}
- name: KAFKA_TOPIC_PREFIX
  value: {{ include "z.topicPrefix" . | quote }}
{{- if include "z.kafkaCertSecret" . }}
- name: KAFKA_CERT_DIR
  value: /etc/kafka-cert
{{- end }}
{{- end -}}

{{/* z.kafkaCertMount / z.kafkaCertVolume — mTLS material for the shared cluster.
     Emit bare list items (no leading newline); wrap call sites in `with` so
     bundled mode renders nothing (not even whitespace). */}}
{{- define "z.kafkaCertMount" -}}
{{- if include "z.kafkaCertSecret" . -}}
- { name: kafka-cert, mountPath: /etc/kafka-cert, readOnly: true }
{{- end -}}
{{- end -}}
{{- define "z.kafkaCertVolume" -}}
{{- with include "z.kafkaCertSecret" . -}}
- name: kafka-cert
  secret: { secretName: {{ . | quote }} }
{{- end -}}
{{- end -}}

{{/* z.mediaClaimName — the PVC holding the media library; every media consumer
     mounts it (addon charts read it as .Values.zaentrum.media.claimName). */}}
{{- define "z.mediaClaimName" -}}media{{- end -}}

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
