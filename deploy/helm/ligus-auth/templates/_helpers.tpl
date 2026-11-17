{{- define "ligus-auth.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ligus-auth.fullname" -}}
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

{{- define "ligus-auth.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "ligus-auth.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ligus
{{- end -}}

{{- define "ligus-auth.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ligus-auth.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "ligus-auth.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ default .Chart.AppVersion .Values.image.tag }}
{{- end -}}
{{- end -}}

{{- define "ligus-auth.config" -}}
{{- $c := deepCopy .Values.config -}}
{{- $_ := set $c "listen" (printf ":%d" (int .Values.service.port)) -}}
{{- if .Values.adminToken.existingSecret -}}
{{- $_ := set $c "admin_listen" (printf ":%d" (int .Values.service.adminPort)) -}}
{{- end -}}
{{- $_ := set $c "signing_key_file" "/etc/ligus-auth/key/signing-key" -}}
{{- if eq (include "ligus-auth.storeDriver" .) "postgres" -}}
{{- $_ := set $c "store" (dict "driver" "postgres" "dsn_env" "LIGUS_AUTH_DATABASE_URL") -}}
{{- else -}}
{{- $_ := set $c "store" (dict "driver" "file" "path" "/var/lib/ligus-auth/state.json") -}}
{{- end -}}
{{- toPrettyJson $c -}}
{{- end -}}

{{/*
checksum/secrets: rolls the pod when a Secret it reads changes. lookup is a
constant under `helm template`; with Argo CD or Flux use Reloader or bump
podAnnotations.
*/}}
{{- define "ligus-auth.secretsChecksum" -}}
{{- $names := list .Values.signingKey.existingSecret -}}
{{- with .Values.secretFiles.existingSecret }}{{ $names = append $names . }}{{ end -}}
{{- with .Values.adminToken.existingSecret }}{{ $names = append $names . }}{{ end -}}
{{- $ctx := include "ligus-auth.dbCtx" . | fromYaml -}}
{{- $_ := set $ctx "root" . -}}
{{- range (include "ligus-postgres.clientSecretNames" $ctx | fromJsonArray) }}{{ $names = append $names . }}{{ end -}}
{{- $data := dict -}}
{{- if include "ligus-postgres.builtDSN" $ctx }}{{ $_ := set $data "dsn" (include "ligus-postgres.dsn" $ctx) }}{{ end -}}
{{- range $n := ($names | compact | uniq | sortAlpha) -}}
{{- $s := lookup "v1" "Secret" $.Release.Namespace $n -}}
{{- $_ := set $data $n (dig "data" (dict) $s) -}}
{{- end -}}
{{- toJson $data | sha256sum -}}
{{- end -}}

{{/*
The effective database settings (YAML) for the ligus-postgres library:
database.*, with the deprecated store.driver=postgres + store.databaseUrl
mapped onto mode=external when database.mode is empty.
*/}}
{{- define "ligus-auth.database" -}}
{{- $v := .Values -}}
{{- /* Chart defaults under what was set: an upgrade with --reuse-values
     carries the previous release's values, which may predate database.*. */ -}}
{{- $defaults := mergeOverwrite (include "ligus-postgres.defaults" . | fromYaml) (dict "name" "ligus_auth" "user" "ligus_auth") -}}
{{- $d := mergeOverwrite (deepCopy $defaults) (deepCopy (default (dict) $v.database)) -}}
{{- $dep := list -}}
{{- $_ := set $d.external "secretName" $d.external.existingSecret -}}
{{- $_ := set $d.external "secretKey" $d.external.existingSecretKey -}}
{{- $_ := set $d.bundled "legacy" false -}}
{{- if not $d.mode -}}
{{- if eq $v.store.driver "postgres" -}}
{{- if not $v.store.databaseUrl.existingSecret -}}
{{- fail "store.driver=postgres needs a database: set database.mode (external, cnpg or bundled), or the deprecated store.databaseUrl.existingSecret" -}}
{{- end -}}
{{- $_ := set $d "mode" "external" -}}
{{- $_ := set $d.external "secretName" $v.store.databaseUrl.existingSecret -}}
{{- $_ := set $d.external "secretKey" $v.store.databaseUrl.key -}}
{{- $dep = append $dep "store.databaseUrl is deprecated: use database.mode=external with database.external.existingSecret (and database.external.cidrs for the egress policy)." -}}
{{- else -}}
{{- $_ := set $d "mode" "none" -}}
{{- end -}}
{{- else if $v.store.databaseUrl.existingSecret -}}
{{- fail "database.mode is set: remove the deprecated store.databaseUrl (it would be ignored)" -}}
{{- end -}}
{{- $_ := set $d "deprecated" $dep -}}
{{- toYaml $d -}}
{{- end -}}

{{/* "postgres" when a database mode is chosen, else "file". */}}
{{- define "ligus-auth.storeDriver" -}}
{{- if eq (include "ligus-auth.database" . | fromYaml).mode "none" -}}file{{- else -}}postgres{{- end -}}
{{- end -}}

{{/* The context dict for the ligus-postgres library (call with the root). */}}
{{- define "ligus-auth.dbCtx" -}}
{{- toYaml (dict
  "db" (include "ligus-auth.database" . | fromYaml)
  "fullname" (include "ligus-auth.fullname" .)
  "labels" (include "ligus-auth.labels" .)
  "dbNameLabel" (printf "%s-postgres" (include "ligus-auth.name" .) | trunc 63)
  "clientSelector" (dict "matchLabels" (include "ligus-auth.selectorLabels" . | fromYaml))
  "mountBase" "/etc/ligus-auth"
  "dsnFile" "database-url"
  "valuesPrefix" "database"
  "networkPolicy" .Values.networkPolicy.enabled
  "restrictEgress" .Values.networkPolicy.restrictEgress
  "dns" .Values.networkPolicy.dns) -}}
{{- end -}}
