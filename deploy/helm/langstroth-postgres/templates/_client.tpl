{{/*
The client side: the pod volume carrying the DSN (and, for cnpg and bundled,
the pgpass line and the server CA), its mount, and the egress rule to the
database. Secrets are files only, mounted 0400.
*/}}

{{- define "langstroth-postgres.clientVolume" -}}
{{- $db := .db -}}
{{- if eq $db.mode "external" }}
- name: db
  secret:
    secretName: {{ $db.external.secretName }}
    defaultMode: 0400
    items:
      - { key: {{ $db.external.secretKey }}, path: {{ .dsnFile }} }
{{- else if and (eq $db.mode "bundled") $db.bundled.legacy }}
# Deprecated postgres.enabled: the DSN (sslmode=disable) from its Secret.
- name: db
  secret:
    secretName: {{ $db.bundled.auth.existingSecret }}
    defaultMode: 0400
    items:
      - { key: DATABASE_URL, path: {{ .dsnFile }} }
{{- else if include "langstroth-postgres.builtDSN" . }}
# The DSN has no password: pgx reads it from pgpass and verifies the server
# against ca.crt. Only those keys are projected (never the CA's private key).
- name: db
  projected:
    defaultMode: 0400
    sources:
      - configMap:
          name: {{ include "langstroth-postgres.dsnConfigMapName" . }}
          items:
            - { key: DATABASE_URL, path: {{ .dsnFile }} }
      {{- if eq $db.mode "cnpg" }}
      - secret:
          name: {{ include "langstroth-postgres.cnpgName" . }}-app
          items:
            - { key: pgpass, path: pgpass }
      - secret:
          name: {{ include "langstroth-postgres.cnpgName" . }}-ca
          items:
            - { key: ca.crt, path: ca.crt }
      {{- else }}
      - secret:
          name: {{ $db.bundled.auth.existingSecret }}
          items:
            - { key: {{ $db.bundled.auth.pgpassKey }}, path: pgpass }
      {{- if include "langstroth-postgres.bundledTLS" . }}
      - secret:
          name: {{ include "langstroth-postgres.tlsSecretName" . }}
          items:
            - { key: {{ $db.bundled.tls.caKey }}, path: ca.crt }
      {{- end }}
      {{- end }}
{{- end }}
{{- end -}}

{{- define "langstroth-postgres.clientMount" -}}
{{- if ne .db.mode "none" }}
- name: db
  mountPath: {{ .mountBase }}/db
  readOnly: true
{{- end }}
{{- end -}}

{{- define "langstroth-postgres.dsnPath" -}}
{{- printf "%s/db/%s" .mountBase .dsnFile -}}
{{- end -}}

{{/* Egress from the client pods to the chosen database only. */}}
{{- define "langstroth-postgres.egressRules" -}}
{{- $db := .db -}}
{{- if eq $db.mode "cnpg" }}
# The CloudNativePG instances of this release's cluster.
- to:
    - podSelector:
        matchLabels:
          cnpg.io/cluster: {{ include "langstroth-postgres.cnpgName" . }}
  ports:
    - { port: 5432, protocol: TCP }
{{- else if eq $db.mode "bundled" }}
# The bundled Postgres.
- to:
    - podSelector:
        matchLabels:
          {{- include "langstroth-postgres.selectorLabels" . | nindent 10 }}
  ports:
    - { port: 5432, protocol: TCP }
{{- else if and (eq $db.mode "external") $db.external.cidrs }}
# The external database (database.external.cidrs; never cloud metadata).
- to:
    {{- range $db.external.cidrs }}
    - ipBlock:
        cidr: {{ . }}
    {{- end }}
  ports:
    - { port: {{ $db.external.port }}, protocol: TCP }
{{- end }}
{{- end -}}

{{/* 1 when the IPv4 cidr (a.b.c.d/p) contains the address ip, else 0. */}}
{{- define "langstroth-postgres.cidrContains" -}}
{{- $parts := splitList "/" .cidr -}}
{{- $p := int (index $parts 1) -}}
{{- $block := 1 -}}
{{- range until (sub 32 $p | int) }}{{ $block = mul $block 2 }}{{ end -}}
{{- $o := splitList "." (index $parts 0) -}}
{{- $n := add (mul (add (mul (add (mul (atoi (index $o 0)) 256) (atoi (index $o 1))) 256) (atoi (index $o 2))) 256) (atoi (index $o 3)) -}}
{{- $a := splitList "." .ip -}}
{{- $m := add (mul (add (mul (add (mul (atoi (index $a 0)) 256) (atoi (index $a 1))) 256) (atoi (index $a 2))) 256) (atoi (index $a 3)) -}}
{{- if eq (div $n $block) (div $m $block) }}1{{ else }}0{{ end -}}
{{- end -}}
