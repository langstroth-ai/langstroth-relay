{{/*
langstroth-postgres: the Postgres choice shared by the langstroth-relay and langstroth-auth
charts. Every template takes one context dict built by the app chart:

  root            the app chart's top-level context ($)
  db              the effective database settings (the app chart resolves
                  deprecated keys into this shape; see its "database" helper)
  fullname        the app's fullname (resource name prefix)
  labels          the app's common labels, rendered
  dbNameLabel     app.kubernetes.io/name for the bundled Postgres pods
  clientSelector  LabelSelector (dict) of the pods that use the database
  mountBase       where the client mounts its files (/etc/langstroth-relay, ...)
  dsnFile         file name of the DSN inside <mountBase>/db
  networkPolicy   whether NetworkPolicies are rendered
  dns             dict namespaceSelector/podSelector for DNS egress

db.mode is one of none, external, cnpg, bundled.
*/}}

{{- define "langstroth-postgres.cnpgName" -}}
{{- printf "%s-db" .fullname | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{/* Same name as the old development pod, so an upgrade keeps its volume. */}}
{{- define "langstroth-postgres.bundledName" -}}
{{- printf "%s-postgres" .fullname | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "langstroth-postgres.backupName" -}}
{{- printf "%s-backup" (include "langstroth-postgres.bundledName" .) | trunc 52 | trimSuffix "-" -}}
{{- end -}}

{{/* The StatefulSet's backup claim (volumeClaimTemplate "backups"). */}}
{{- define "langstroth-postgres.backupClaim" -}}
{{- printf "backups-%s-0" (include "langstroth-postgres.bundledName" .) -}}
{{- end -}}

{{- define "langstroth-postgres.dsnConfigMapName" -}}
{{- printf "%s-dsn" .fullname | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "langstroth-postgres.tlsSecretName" -}}
{{- $t := .db.bundled.tls -}}
{{- if $t.existingSecret -}}
{{- $t.existingSecret -}}
{{- else -}}
{{- printf "%s-tls" (include "langstroth-postgres.bundledName" .) -}}
{{- end -}}
{{- end -}}

{{- define "langstroth-postgres.selectorLabels" -}}
app.kubernetes.io/name: {{ .dbNameLabel }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: postgres
{{- end -}}

{{- define "langstroth-postgres.backupSelectorLabels" -}}
app.kubernetes.io/name: {{ .dbNameLabel }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: postgres-backup
{{- end -}}

{{/* "true" when the client reads a DSN the chart builds (no password in it). */}}
{{- define "langstroth-postgres.builtDSN" -}}
{{- if or (eq .db.mode "cnpg") (and (eq .db.mode "bundled") (not .db.bundled.legacy)) -}}true{{- end -}}
{{- end -}}

{{/* "true" when the bundled Postgres serves TLS. */}}
{{- define "langstroth-postgres.bundledTLS" -}}
{{- $t := .db.bundled.tls -}}
{{- if and (eq .db.mode "bundled") (not .db.bundled.legacy) (or $t.existingSecret $t.certManager.issuerRef) -}}true{{- end -}}
{{- end -}}

{{- define "langstroth-postgres.host" -}}
{{- if eq .db.mode "cnpg" -}}
{{- printf "%s-rw" (include "langstroth-postgres.cnpgName" .) -}}
{{- else -}}
{{- include "langstroth-postgres.bundledName" . -}}
{{- end -}}
{{- end -}}

{{/*
The DSN for cnpg and bundled: no password. pgx (relay and langstroth-auth) reads
the password from passfile and verifies the server against sslrootcert, both
mounted next to the DSN, so no secret is in the environment or in a value.
*/}}
{{- define "langstroth-postgres.dsn" -}}
{{- $dir := printf "%s/db" .mountBase -}}
{{- $q := list -}}
{{- if or (eq .db.mode "cnpg") (include "langstroth-postgres.bundledTLS" .) -}}
{{- $q = append $q "sslmode=verify-full" -}}
{{- $q = append $q (printf "sslrootcert=%s/ca.crt" $dir) -}}
{{- else -}}
{{- $q = append $q "sslmode=disable" -}}
{{- end -}}
{{- $q = append $q (printf "passfile=%s/pgpass" $dir) -}}
{{- printf "postgres://%s@%s:5432/%s?%s" (urlquery .db.user) (include "langstroth-postgres.host" .) (urlquery .db.name) (join "&" $q) -}}
{{- end -}}

{{/* "true" while a bundled restore is pending: the app scales to zero. */}}
{{- define "langstroth-postgres.restoring" -}}
{{- if and (eq .db.mode "bundled") .db.bundled.backups.restore.file -}}true{{- end -}}
{{- end -}}

{{/* Secret names the client reads (for checksum/secrets). */}}
{{- define "langstroth-postgres.clientSecretNames" -}}
{{- $n := list -}}
{{- if eq .db.mode "external" -}}
{{- $n = append $n .db.external.secretName -}}
{{- else if eq .db.mode "cnpg" -}}
{{- $c := include "langstroth-postgres.cnpgName" . -}}
{{- $n = append $n (printf "%s-app" $c) -}}
{{- $n = append $n (printf "%s-ca" $c) -}}
{{- else if eq .db.mode "bundled" -}}
{{- $n = append $n .db.bundled.auth.existingSecret -}}
{{- if include "langstroth-postgres.bundledTLS" . -}}
{{- $n = append $n (include "langstroth-postgres.tlsSecretName" .) -}}
{{- end -}}
{{- end -}}
{{- toJson $n -}}
{{- end -}}
