{{/*
Shared conventions for the zaentrum platform chart. Every service template uses
these so image/issuer/hostAliases/pull-secrets/labels stay consistent.
*/}}

{{/* z.issuerScheme — the scheme the platform derives its URLs with: https
     with a certificate of its own (tls.enabled), else identity.issuerScheme. */}}
{{- define "z.issuerScheme" -}}
{{- if .Values.tls.enabled -}}https{{- else -}}{{ .Values.identity.issuerScheme }}{{- end -}}
{{- end -}}

{{/* z.issuer — the OIDC issuer URL (explicit override, else derived). */}}
{{- define "z.issuer" -}}
{{- if .Values.identity.issuer -}}
{{- .Values.identity.issuer -}}
{{- else -}}
{{- include "z.issuerScheme" . }}://{{ .Values.global.hostname }}/auth/realms/zaentrum
{{- end -}}
{{- end -}}

{{/* z.kcHostname — KC_HOSTNAME for the bundled Keycloak (scheme+host+/auth). */}}
{{- define "z.kcHostname" -}}
{{- include "z.issuerScheme" . }}://{{ .Values.global.hostname }}/auth
{{- end -}}

{{/* z.tlsSecret — the Secret the platform's certificate is in. */}}
{{- define "z.tlsSecret" -}}
{{- .Values.tls.secretName | default "zaentrum-tls" -}}
{{- end -}}

{{/* z.tlsHosts — the hosts the certificate is for, as a JSON list: the
     platform's, and the chino host of subdomains routing. */}}
{{- define "z.tlsHosts" -}}
{{- $hosts := list .Values.global.hostname -}}
{{- if and (eq .Values.routing.mode "subdomains") .Values.routing.hosts.chino -}}
{{- $hosts = append $hosts .Values.routing.hosts.chino -}}
{{- end -}}
{{- toJson $hosts -}}
{{- end -}}

{{/* z.tlsPEM — one PEM of the platform's certificate for the Routes, which
     carry it inline: tls.<value> when the operator passed it, else the
     Secret's <key> (lookup, under helm install/upgrade). Empty without
     tls.enabled, or before the Secret is there: the router's own then.
     Use: (dict "root" $ "value" "certificate" "key" "tls.crt"). */}}
{{- define "z.tlsPEM" -}}
{{- if .root.Values.tls.enabled -}}
{{- $pem := index .root.Values.tls .value -}}
{{- if not $pem -}}
{{- $live := lookup "v1" "Secret" .root.Release.Namespace (include "z.tlsSecret" .root) -}}
{{- if and $live $live.data -}}{{- with index $live.data .key -}}{{- $pem = b64dec . -}}{{- end -}}{{- end -}}
{{- end -}}
{{- $pem -}}
{{- end -}}
{{- end -}}

{{/* z.routeTLS — a Route's tls: edge termination, http as policy says, and
     the platform's certificate when there is one (tls.enabled), else the
     router's. Place under spec: {{- include "z.routeTLS" (dict "root" $ "policy" "Allow") | nindent 2 }} */}}
{{- define "z.routeTLS" -}}
{{- $crt := include "z.tlsPEM" (dict "root" .root "value" "certificate" "key" "tls.crt") -}}
{{- $key := include "z.tlsPEM" (dict "root" .root "value" "key" "key" "tls.key") -}}
{{- if and $crt $key }}
tls:
  termination: edge
  insecureEdgeTerminationPolicy: {{ .policy }}
  certificate: {{ $crt | quote }}
  key: {{ $key | quote }}
{{- with include "z.tlsPEM" (dict "root" .root "value" "caCertificate" "key" "ca.crt") }}
  caCertificate: {{ . | quote }}
{{- end }}
{{- else }}
tls: { termination: edge, insecureEdgeTerminationPolicy: {{ .policy }} }
{{- end }}
{{- end -}}

{{/* z.publicURL — the origin users reach the platform at: https where the edge
     terminates TLS (OpenShift Routes always do; issuerScheme https says a proxy
     in front does), else http. The verification Job checks the platform from
     there, outside-in, the way a user meets it. */}}
{{- define "z.publicURL" -}}
{{- if or (eq (include "z.issuerScheme" .) "https") .Values.routing.provisionRoutes -}}https{{- else -}}http{{- end -}}://{{ .Values.global.hostname }}
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
z.realmImport — the bundled realm as a new one is imported, as JSON:
files/keycloak-realm.json with the redirects, web origins and post-logout
redirect URIs of z.realmClients filled in, and files/user-profile.json as the
realm's user profile. The import (keycloak-realm.yaml) writes it into a new
realm; the realm Job (realm.yaml) makes a client an existing realm lacks from
it, and sets what z.realmSettings, z.peopleClient and z.ratingMapper read from
it.
*/}}
{{- define "z.realmImport" -}}
{{- $realm := .Files.Get "files/keycloak-realm.json" | fromJson }}
{{- $want := include "z.realmClients" . | fromJson }}
{{- range $client := $realm.clients }}
{{- with index $want $client.clientId }}
{{- $_ := set $client "redirectUris" .redirectUris }}
{{- $_ := set $client "webOrigins" .webOrigins }}
{{- if not $client.attributes }}{{ $_ := set $client "attributes" dict }}{{ end }}
{{- range $k, $v := .attributes }}{{ $_ := set $client.attributes $k $v }}{{ end }}
{{- end }}
{{- end }}
{{- /* Keycloak keeps a realm's user profile as a component whose config holds
       the profile as one JSON string; a new realm without one gets Keycloak's
       default, which keeps no attribute it does not declare. */}}
{{- $profile := dict "providerId" "declarative-user-profile" "subComponents" dict
      "config" (dict "kc.user.profile.config" (list (.Files.Get "files/user-profile.json" | fromJson | toJson))) }}
{{- $_ := set $realm "components" (dict "org.keycloak.userprofile.UserProfileProvider" (list $profile)) }}
{{- toJson $realm -}}
{{- end -}}

{{/*
z.peopleClient — the client portal-api manages the realm's people with (the
People page and its invites, server/internal/people in zaentrum-portal), as
JSON: its clientId, its representation in the realm import, the settings the
realm Job keeps (name=value, as z.realmSettings), and the realm-management
roles its service account holds — those three and nothing else. Pass the
realm import (z.realmImport, parsed).
*/}}
{{- define "z.peopleClient" -}}
{{- $import := . -}}
{{- $rep := dict -}}
{{- range $import.clients }}{{ if eq .clientId "zaentrum-people" }}{{ $rep = . }}{{ end }}{{ end -}}
{{- $roles := list -}}
{{- range $import.users }}{{ if eq (.serviceAccountClientId | default "") "zaentrum-people" }}{{ $roles = index .clientRoles "realm-management" }}{{ end }}{{ end -}}
{{- $settings := list -}}
{{- range $field := list "publicClient" "serviceAccountsEnabled" "standardFlowEnabled" "implicitFlowEnabled" "directAccessGrantsEnabled" -}}
{{- $settings = append $settings (printf "%s=%v" $field (index $rep $field)) -}}
{{- end -}}
{{- $settings = append $settings (printf "oauth2.device.authorization.grant.enabled=%s" (index $rep.attributes "oauth2.device.authorization.grant.enabled")) -}}
{{- dict "clientId" $rep.clientId "representation" $rep "settings" (join " " $settings) "roles" (join " " (sortAlpha $roles)) | toJson -}}
{{- end -}}

{{/*
z.ratingMapper — the protocol mapper that puts a person's rating cap, the user
attribute max_rating (an age), into the access tokens of the clients people
watch through, as the claim max_rating (an integer; no claim, no cap): the
clients of the realm import that carry it, its representation there, and its
config as the realm Job checks it (name=value). And the attribute itself, as
the realm's user profile declares it (files/user-profile.json): its
representation, and the part of it the realm Job checks — who may see and
change it, the range it takes, whether it is required, single-valued — in the
order Keycloak answers a projection of it. Pass the realm import
(z.realmImport, parsed).
*/}}
{{- define "z.ratingMapper" -}}
{{- $import := . -}}
{{- $clients := list -}}
{{- $mapper := dict -}}
{{- range $c := $import.clients -}}
{{- range $m := $c.protocolMappers | default list -}}
{{- if eq $m.name "max-rating" -}}{{ $clients = append $clients $c.clientId }}{{ $mapper = $m }}{{- end -}}
{{- end -}}
{{- end -}}
{{- $config := list -}}
{{- range $k, $v := $mapper.config }}{{ $config = append $config (printf "%s=%s" $k $v) }}{{ end -}}
{{- $attribute := dict -}}
{{- $profile := index (index $import.components "org.keycloak.userprofile.UserProfileProvider") 0 -}}
{{- range (index $profile.config "kc.user.profile.config" | first | fromJson).attributes }}{{ if eq .name "max_rating" }}{{ $attribute = . }}{{ end }}{{ end -}}
{{- $check := printf "{\"name\":%s,\"validations\":%s,\"permissions\":{\"view\":%s,\"edit\":%s},\"multivalued\":%v}"
      (toJson $attribute.name) (toJson $attribute.validations) (toJson $attribute.permissions.view) (toJson $attribute.permissions.edit) $attribute.multivalued -}}
{{- dict "clients" (join " " (sortAlpha $clients)) "mapper" $mapper "config" (join " " $config) "attribute" $attribute "check" $check | toJson -}}
{{- end -}}

{{/*
z.realmSettings — how a client of the realm import signs people in, as the
realm Job sets it: space-separated name=value, a client field or, with a dot
in its name, an attribute. Pass the client's representation. A public client
with no secret, the flows it allows and none other, and PKCE (S256) on them.
*/}}
{{- define "z.realmSettings" -}}
{{- $c := . -}}
{{- $a := .attributes | default dict -}}
{{- range $i, $field := list "publicClient" "standardFlowEnabled" "implicitFlowEnabled" "directAccessGrantsEnabled" -}}
{{- if $i }} {{ end }}{{ $field }}={{ required (printf "the realm import's client %s says nothing of %s" $c.clientId $field) (index $c $field) }}
{{- end }} oauth2.device.authorization.grant.enabled={{ index $a "oauth2.device.authorization.grant.enabled" | default "false" }} pkce.code.challenge.method={{ required (printf "the realm import's client %s has no PKCE method" $c.clientId) (index $a "pkce.code.challenge.method") }}
{{- end -}}

{{/*
z.tvClientId / z.mobileClientId — the public clients chino-api advertises in
/api/config for the TV apps and for the phone and tablet apps: identity.
tvClientId / mobileClientId, else, with bundled identity, the bundled realm's
own (chino-tv, chino-mobile). Empty with an external provider and nothing
set: chino-api's default applies.
*/}}
{{- define "z.tvClientId" -}}
{{- .Values.identity.tvClientId | default (ternary "chino-tv" "" (eq .Values.identity.mode "bundled")) -}}
{{- end -}}
{{- define "z.mobileClientId" -}}
{{- .Values.identity.mobileClientId | default (ternary "chino-mobile" "" (eq .Values.identity.mode "bundled")) -}}
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

{{/*
z.backupEnabled — "true" when the bundled Postgres is backed up:
backup.enabled, else wherever it keeps its data on a claim (z.postgresVolume)
— a new install — and not on an emptyDir. Empty with external databases.
*/}}
{{- define "z.backupEnabled" -}}
{{- if ne .Values.databases.mode "external" -}}
{{- $on := .Values.backup.enabled -}}
{{- if kindIs "invalid" $on -}}{{- $on = ne (include "z.postgresVolume" .) "emptyDir" -}}{{- end -}}
{{- if $on -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{/* z.backupClaim — the claim the dumps are kept on. */}}
{{- define "z.backupClaim" -}}
{{- .Values.backup.claimName | default "backups" -}}
{{- end -}}

{{/* z.restoring — the dump a restore restores the bundled Postgres from this
     render, or nothing. While it does, every client of the database is
     stopped (z.replicas "db") and no backup starts. */}}
{{- define "z.restoring" -}}
{{- if ne .Values.databases.mode "external" -}}{{ .Values.backup.restore }}{{- end -}}
{{- end -}}

{{/* z.platformDatabases — the platform's databases, space-separated. */}}
{{- define "z.platformDatabases" -}}
{{- with .Values.databases -}}{{ .chino }} {{ .katalog }} {{ .keycloak }} {{ .portal }}{{- end -}}
{{- end -}}

{{/* z.backupSecurity — the pod securityContext of the backup and restore
     Jobs: z.podSecurity, and off OpenShift the group 65532 owns what they
     write, for a volume whose root it would not be. */}}
{{- define "z.backupSecurity" -}}
securityContext:
  runAsNonRoot: true
{{- if not (.Capabilities.APIVersions.Has "security.openshift.io/v1") }}
  runAsUser: 65532
  fsGroup: 65532
{{- end }}
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{/* z.partOf — the app.kubernetes.io/part-of label value. */}}
{{- define "z.partOf" -}}{{ .Values.global.partOf }}{{- end -}}

{{/*
z.replicas — per-service replica count from .Values.services.<name>.replicas,
falling back to a default. (Avoids Sprig `dig`, which rejects the typed
chartutil.Values.) Use: replicas: {{ include "z.replicas" (dict "root" $ "name" "chino-api" "def" 1) }}
"db" true marks a service of the bundled Postgres's — one with a session in a
database, a reader as much as a writer, or one that writes through
katalog-manager-api: while a restore recreates the databases (z.restoring),
which no session may hold, it is 0.
*/}}
{{- define "z.replicas" -}}
{{- $r := .def -}}
{{- $svcs := .root.Values.services -}}
{{- if $svcs -}}
{{- $svc := index $svcs .name -}}
{{- if $svc -}}{{- $r = ($svc.replicas | default .def) -}}{{- end -}}
{{- end -}}
{{- if and .db (include "z.restoring" .root) -}}{{- $r = 0 -}}{{- end -}}
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
