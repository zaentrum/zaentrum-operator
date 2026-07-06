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
