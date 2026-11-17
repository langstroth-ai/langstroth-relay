{{/* Every database resource for the chosen mode (YAML documents). */}}
{{- define "ligus-postgres.resources" -}}
{{- include "ligus-postgres.validate" . -}}
{{- if include "ligus-postgres.builtDSN" . }}
---
# The DSN without a password (database.mode={{ .db.mode }}). Not a secret: the
# password is the pgpass file and the CA is ca.crt, both mounted from Secrets.
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "ligus-postgres.dsnConfigMapName" . }}
  labels:
    {{- .labels | nindent 4 }}
data:
  DATABASE_URL: {{ include "ligus-postgres.dsn" . | quote }}
{{- end }}
{{- if eq .db.mode "cnpg" }}
{{ include "ligus-postgres.cnpg" . }}
{{- else if eq .db.mode "bundled" }}
{{ include "ligus-postgres.bundled" . }}
{{- end }}
{{- end -}}

{{/* Install notes for the database. */}}
{{- define "ligus-postgres.notes" -}}
{{- $db := .db -}}
{{- $p := .valuesPrefix -}}
{{- range $db.deprecated }}

DEPRECATED: {{ . }}
{{- end }}
{{- if eq $db.mode "cnpg" }}

Database: CloudNativePG cluster {{ include "ligus-postgres.cnpgName" . }} ({{ int $db.cnpg.instances }} instance{{ if gt (int $db.cnpg.instances) 1 }}s{{ end }}), TLS verify-full against the operator's CA.
{{- if not $db.cnpg.backups.enabled }}
WARNING: backups are off ({{ $p }}.cnpg.backups.acknowledgeNoBackups).
{{- end }}
{{- else if eq $db.mode "bundled" }}

Database: bundled single-instance Postgres {{ include "ligus-postgres.bundledName" . }} (no HA).
{{- if not (include "ligus-postgres.bundledTLS" .) }}
WARNING: plaintext connections inside the namespace ({{ if $db.bundled.legacy }}postgres.enabled{{ else }}{{ $p }}.bundled.allowPlaintextInNamespace{{ end }}). Development only.
{{- end }}
{{- if $db.bundled.backups.enabled }}
Backups: pg_dump on "{{ $db.bundled.backups.schedule }}" to PVC {{ include "ligus-postgres.backupClaim" . }}, newest {{ int $db.bundled.backups.keep }} kept. Copy them off the cluster too.
{{- else }}
WARNING: no backups.
{{- end }}
{{- if include "ligus-postgres.restoring" . }}

RESTORING {{ $db.bundled.backups.restore.file }}: the app runs no replicas and the backup CronJob is suspended. When the restore Job has succeeded, unset {{ $p }}.bundled.backups.restore.file and upgrade again.
{{- end }}
{{- else if eq $db.mode "external" }}
{{- if and .networkPolicy .restrictEgress (not $db.external.cidrs) }}

NOTE: egress is restricted and {{ $p }}.external.cidrs is empty: list the database's address there (or in networkPolicy.extraEgress), or the pods cannot reach it.
{{- end }}
{{- end }}
{{- end -}}
