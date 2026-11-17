{{/*
ligus-postgres: the Postgres choice shared by the ligus-relay and ligus-auth
charts. Every template takes one context dict built by the app chart:

  root            the app chart's top-level context ($)
  db              the effective database settings (the app chart resolves
                  deprecated keys into this shape; see its "database" helper)
  fullname        the app's fullname (resource name prefix)
  labels          the app's common labels, rendered
  dbNameLabel     app.kubernetes.io/name for the bundled Postgres pods
  clientSelector  LabelSelector (dict) of the pods that use the database
  mountBase       where the client mounts its files (/etc/ligus-relay, ...)
  dsnFile         file name of the DSN inside <mountBase>/db
  networkPolicy   whether NetworkPolicies are rendered
  dns             dict namespaceSelector/podSelector for DNS egress

db.mode is one of none, external, cnpg, bundled.
*/}}

{{- define "ligus-postgres.cnpgName" -}}
{{- printf "%s-db" .fullname | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{/* Same name as the old development pod, so an upgrade keeps its volume. */}}
{{- define "ligus-postgres.bundledName" -}}
{{- printf "%s-postgres" .fullname | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ligus-postgres.backupName" -}}
{{- printf "%s-backup" (include "ligus-postgres.bundledName" .) | trunc 52 | trimSuffix "-" -}}
{{- end -}}

{{/* The StatefulSet's backup claim (volumeClaimTemplate "backups"). */}}
{{- define "ligus-postgres.backupClaim" -}}
{{- printf "backups-%s-0" (include "ligus-postgres.bundledName" .) -}}
{{- end -}}

{{- define "ligus-postgres.dsnConfigMapName" -}}
{{- printf "%s-dsn" .fullname | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ligus-postgres.tlsSecretName" -}}
{{- $t := .db.bundled.tls -}}
{{- if $t.existingSecret -}}
{{- $t.existingSecret -}}
{{- else -}}
{{- printf "%s-tls" (include "ligus-postgres.bundledName" .) -}}
{{- end -}}
{{- end -}}

{{- define "ligus-postgres.selectorLabels" -}}
app.kubernetes.io/name: {{ .dbNameLabel }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: postgres
{{- end -}}

{{- define "ligus-postgres.backupSelectorLabels" -}}
app.kubernetes.io/name: {{ .dbNameLabel }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: postgres-backup
{{- end -}}

{{/* "true" when the client reads a DSN the chart builds (no password in it). */}}
{{- define "ligus-postgres.builtDSN" -}}
{{- if or (eq .db.mode "cnpg") (and (eq .db.mode "bundled") (not .db.bundled.legacy)) -}}true{{- end -}}
{{- end -}}

{{/* "true" when the bundled Postgres serves TLS. */}}
{{- define "ligus-postgres.bundledTLS" -}}
{{- $t := .db.bundled.tls -}}
{{- if and (eq .db.mode "bundled") (not .db.bundled.legacy) (or $t.existingSecret $t.certManager.issuerRef) -}}true{{- end -}}
{{- end -}}

{{- define "ligus-postgres.host" -}}
{{- if eq .db.mode "cnpg" -}}
{{- printf "%s-rw" (include "ligus-postgres.cnpgName" .) -}}
{{- else -}}
{{- include "ligus-postgres.bundledName" . -}}
{{- end -}}
{{- end -}}

{{/*
The DSN for cnpg and bundled: no password. pgx (relay and ligus-auth) reads
the password from passfile and verifies the server against sslrootcert, both
mounted next to the DSN, so no secret is in the environment or in a value.
*/}}
{{- define "ligus-postgres.dsn" -}}
{{- $dir := printf "%s/db" .mountBase -}}
{{- $q := list -}}
{{- if or (eq .db.mode "cnpg") (include "ligus-postgres.bundledTLS" .) -}}
{{- $q = append $q "sslmode=verify-full" -}}
{{- $q = append $q (printf "sslrootcert=%s/ca.crt" $dir) -}}
{{- else -}}
{{- $q = append $q "sslmode=disable" -}}
{{- end -}}
{{- $q = append $q (printf "passfile=%s/pgpass" $dir) -}}
{{- printf "postgres://%s@%s:5432/%s?%s" (urlquery .db.user) (include "ligus-postgres.host" .) (urlquery .db.name) (join "&" $q) -}}
{{- end -}}

{{/* "true" while a bundled restore is pending: the app scales to zero. */}}
{{- define "ligus-postgres.restoring" -}}
{{- if and (eq .db.mode "bundled") .db.bundled.backups.restore.file -}}true{{- end -}}
{{- end -}}

{{/* Secret names the client reads (for checksum/secrets). */}}
{{- define "ligus-postgres.clientSecretNames" -}}
{{- $n := list -}}
{{- if eq .db.mode "external" -}}
{{- $n = append $n .db.external.secretName -}}
{{- else if eq .db.mode "cnpg" -}}
{{- $c := include "ligus-postgres.cnpgName" . -}}
{{- $n = append $n (printf "%s-app" $c) -}}
{{- $n = append $n (printf "%s-ca" $c) -}}
{{- else if eq .db.mode "bundled" -}}
{{- $n = append $n .db.bundled.auth.existingSecret -}}
{{- if include "ligus-postgres.bundledTLS" . -}}
{{- $n = append $n (include "ligus-postgres.tlsSecretName" .) -}}
{{- end -}}
{{- end -}}
{{- toJson $n -}}
{{- end -}}
