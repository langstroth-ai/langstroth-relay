{{/*
database.mode=bundled: one Postgres instance in the release namespace for
small self-hosts. No HA: the pod and its volume are a single point of
failure; the pg_dump CronJob is the recovery path.

TLS is required (server certificate from a Secret or a cert-manager
Certificate); the application role is not a superuser; pg_hba admits it over
TLS with scram-sha-256 only, and the superuser only over the local socket
(peer). The deprecated postgres.enabled alias (db.bundled.legacy) keeps the
old development pod unchanged: application role as superuser, no TLS.
*/}}
{{- define "ligus-postgres.bundled" -}}
{{- $db := .db -}}
{{- $b := $db.bundled -}}
{{- $name := include "ligus-postgres.bundledName" . -}}
{{- $tls := include "ligus-postgres.bundledTLS" . -}}
{{- $legacy := $b.legacy -}}
{{- $ctx := . }}
---
apiVersion: v1
kind: Service
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
spec:
  type: ClusterIP
  ports:
    - name: postgres
      port: 5432
      targetPort: postgres
  selector:
    {{- include "ligus-postgres.selectorLabels" . | nindent 4 }}
{{- if not $legacy }}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ $name }}-conf
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
data:
  {{- include "ligus-postgres.bundledConf" . | nindent 2 }}
{{- end }}
{{- if and $tls $b.tls.certManager.issuerRef }}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
spec:
  secretName: {{ include "ligus-postgres.tlsSecretName" . }}
  issuerRef:
    {{- toYaml $b.tls.certManager.issuerRef | nindent 4 }}
  {{- with $b.tls.certManager.duration }}
  duration: {{ . }}
  {{- end }}
  {{- with $b.tls.certManager.renewBefore }}
  renewBefore: {{ . }}
  {{- end }}
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
  usages: [server auth]
  commonName: {{ $name }}
  dnsNames:
    - {{ $name }}
    - {{ $name }}.{{ .root.Release.Namespace }}
    - {{ $name }}.{{ .root.Release.Namespace }}.svc
    - {{ $name }}.{{ .root.Release.Namespace }}.svc.cluster.local
{{- end }}
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
spec:
  serviceName: {{ $name }}
  # One instance; there is no replica to fail over to.
  replicas: 1
  selector:
    matchLabels:
      {{- include "ligus-postgres.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "ligus-postgres.selectorLabels" . | nindent 8 }}
      {{- if not $legacy }}
      annotations:
        checksum/conf: {{ include "ligus-postgres.bundledConf" . | sha256sum }}
      {{- end }}
    spec:
      automountServiceAccountToken: false
      enableServiceLinks: false
      securityContext:
        runAsNonRoot: true
        # The postgres user of the official Alpine image.
        runAsUser: 70
        runAsGroup: 70
        fsGroup: 70
        seccompProfile:
          type: RuntimeDefault
      {{- with $b.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with $b.tolerations }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      containers:
        - name: postgres
          image: "{{ $b.image.repository }}:{{ $b.image.tag }}"
          imagePullPolicy: {{ $b.image.pullPolicy }}
          {{- if not $legacy }}
          args:
            - postgres
            - -c
            - hba_file=/etc/postgres-conf/pg_hba.conf
            - -c
            - password_encryption=scram-sha-256
            {{- if $tls }}
            - -c
            - ssl=on
            - -c
            - ssl_cert_file=/etc/postgres-tls/tls.crt
            - -c
            - ssl_key_file=/etc/postgres-tls/tls.key
            - -c
            - ssl_min_protocol_version=TLSv1.2
            {{- end }}
            {{- range $k, $v := $b.parameters }}
            - -c
            - {{ printf "%s=%v" $k $v | quote }}
            {{- end }}
          {{- end }}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          ports:
            - name: postgres
              containerPort: 5432
          env:
            - name: POSTGRES_DB
              value: {{ $db.name | quote }}
            - name: POSTGRES_USER
              value: {{ if $legacy }}{{ $db.user | quote }}{{ else }}postgres{{ end }}
            - name: POSTGRES_PASSWORD_FILE
              value: /etc/postgres-secret/password
            - name: PGDATA
              value: /var/lib/postgresql/data/pgdata
            {{- if not $legacy }}
            - name: POSTGRES_INITDB_ARGS
              value: "--auth-local=peer --auth-host=scram-sha-256"
            - name: LIGUS_DB_USER
              value: {{ $db.user | quote }}
            {{- end }}
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", {{ if $legacy }}{{ $db.user | quote }}{{ else }}postgres{{ end }}, "-d", {{ $db.name | quote }}]
            periodSeconds: 5
          livenessProbe:
            exec:
              command: ["pg_isready", "-U", {{ if $legacy }}{{ $db.user | quote }}{{ else }}postgres{{ end }}]
            periodSeconds: 20
            failureThreshold: 6
          resources:
            {{- toYaml $b.resources | nindent 12 }}
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
            - name: run
              mountPath: /var/run/postgresql
            - name: tmp
              mountPath: /tmp
            - name: secret
              mountPath: /etc/postgres-secret
              readOnly: true
            {{- if not $legacy }}
            - name: conf
              mountPath: /etc/postgres-conf
              readOnly: true
            - name: initdb
              mountPath: /docker-entrypoint-initdb.d
              readOnly: true
            {{- end }}
            {{- if $tls }}
            - name: tls
              mountPath: /etc/postgres-tls
              readOnly: true
            {{- end }}
        {{- if $tls }}
        # Postgres reads its certificate at start and on reload: reload when
        # the mounted Secret changes (cert-manager renewals, a rotated
        # Secret), as the postgres OS user over the local socket (peer).
        - name: tls-reload
          image: "{{ $b.image.repository }}:{{ $b.image.tag }}"
          imagePullPolicy: {{ $b.image.pullPolicy }}
          command: ["/bin/sh", "-c"]
          args:
            - |
              sum() { cat /etc/postgres-tls/tls.crt /etc/postgres-tls/tls.key | md5sum; }
              last=$(sum)
              while sleep 60; do
                cur=$(sum)
                if [ "$cur" != "$last" ] && psql -h /var/run/postgresql -U postgres -d postgres -Atqc 'SELECT pg_reload_conf()' >/dev/null; then
                  echo "certificate changed: configuration reloaded"; last=$cur
                fi
              done
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          resources:
            requests: { cpu: 5m, memory: 8Mi }
            limits: { memory: 32Mi }
          volumeMounts:
            - name: run
              mountPath: /var/run/postgresql
            - name: tls
              mountPath: /etc/postgres-tls
              readOnly: true
        {{- end }}
      volumes:
        - name: secret
          secret:
            secretName: {{ $b.auth.existingSecret }}
            defaultMode: 0400
            items:
              - { key: password, path: password }
        {{- if not $legacy }}
        - name: conf
          configMap:
            name: {{ $name }}-conf
            items:
              - { key: pg_hba.conf, path: pg_hba.conf }
        - name: initdb
          configMap:
            name: {{ $name }}-conf
            items:
              - { key: 10-app-role.sh, path: 10-app-role.sh }
        {{- end }}
        {{- if $tls }}
        # Postgres accepts a key owned by root with group read (0440 with
        # fsGroup), not world-readable.
        - name: tls
          secret:
            secretName: {{ include "ligus-postgres.tlsSecretName" . }}
            defaultMode: 0400
            items:
              - { key: tls.crt, path: tls.crt }
              - { key: tls.key, path: tls.key }
        {{- end }}
        - name: run
          emptyDir: {}
        - name: tmp
          emptyDir: {}
        {{- if not $b.persistence.enabled }}
        - name: data
          emptyDir: {}
        {{- end }}
  {{- if or $b.persistence.enabled $b.backups.enabled }}
  volumeClaimTemplates:
    {{- if $b.persistence.enabled }}
    - metadata:
        name: data
      spec:
        accessModes:
          {{- toYaml $b.persistence.accessModes | nindent 10 }}
        {{- with $b.persistence.storageClass }}
        storageClassName: {{ . }}
        {{- end }}
        resources:
          requests:
            storage: {{ $b.persistence.size }}
    {{- end }}
    {{- if $b.backups.enabled }}
    # The dumps' volume (backups-<name>-0). A claim of the StatefulSet, so it
    # binds with the Postgres pod (a standalone claim with
    # WaitForFirstConsumer stays Pending until the first backup, which holds
    # up helm --wait and GitOps health) and lives on its node, where the
    # backup jobs run. Not mounted by Postgres; kept when the StatefulSet or
    # the release is deleted.
    - metadata:
        name: backups
      spec:
        accessModes:
          {{- toYaml $b.backups.persistence.accessModes | nindent 10 }}
        {{- with $b.backups.persistence.storageClass }}
        storageClassName: {{ . }}
        {{- end }}
        resources:
          requests:
            storage: {{ $b.backups.persistence.size }}
    {{- end }}
  {{- end }}
{{- if $b.backups.enabled }}
{{ include "ligus-postgres.bundledBackups" . }}
{{- end }}
{{- if .networkPolicy }}
---
# Only the clients and the backup job reach 5432; Postgres itself opens no
# connections.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
spec:
  podSelector:
    matchLabels:
      {{- include "ligus-postgres.selectorLabels" . | nindent 6 }}
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - podSelector:
            {{- toYaml .clientSelector | nindent 12 }}
        {{- if $b.backups.enabled }}
        - podSelector:
            matchLabels:
              {{- include "ligus-postgres.backupSelectorLabels" . | nindent 14 }}
        {{- end }}
      ports:
        - { port: 5432, protocol: TCP }
  egress: []
{{- if $b.backups.enabled }}
---
# The backup and restore jobs: DNS, then Postgres, nothing else.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: {{ include "ligus-postgres.backupName" . }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres-backup
spec:
  podSelector:
    matchLabels:
      {{- include "ligus-postgres.backupSelectorLabels" . | nindent 6 }}
  policyTypes: [Ingress, Egress]
  ingress: []
  egress:
    - to:
        - namespaceSelector:
            {{- toYaml .dns.namespaceSelector | nindent 12 }}
          podSelector:
            {{- toYaml .dns.podSelector | nindent 12 }}
      ports:
        - { port: 53, protocol: UDP }
        - { port: 53, protocol: TCP }
    - to:
        - podSelector:
            matchLabels:
              {{- include "ligus-postgres.selectorLabels" . | nindent 14 }}
      ports:
        - { port: 5432, protocol: TCP }
{{- end }}
{{- end }}
{{- end -}}

{{/* The pod spec shared by the backup CronJob and the restore Job. */}}
{{- define "ligus-postgres.jobPod" -}}
{{- $db := .ctx.db -}}
{{- $b := $db.bundled -}}
{{- $tls := include "ligus-postgres.bundledTLS" .ctx -}}
metadata:
  labels:
    {{- include "ligus-postgres.backupSelectorLabels" .ctx | nindent 4 }}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  enableServiceLinks: false
  # On the Postgres pod's node: the backup volume is attached there.
  affinity:
    podAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - topologyKey: kubernetes.io/hostname
          labelSelector:
            matchLabels:
              {{- include "ligus-postgres.selectorLabels" .ctx | nindent 14 }}
  securityContext:
    runAsNonRoot: true
    runAsUser: 70
    runAsGroup: 70
    fsGroup: 70
    seccompProfile:
      type: RuntimeDefault
  {{- with $b.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $b.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
    - name: {{ .container }}
      image: "{{ $b.image.repository }}:{{ $b.image.tag }}"
      imagePullPolicy: {{ $b.image.pullPolicy }}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      # Connection settings only; the password is a mounted file.
      env:
        - { name: PGHOST, value: {{ include "ligus-postgres.bundledName" .ctx | quote }} }
        - { name: PGPORT, value: "5432" }
        - { name: PGDATABASE, value: {{ $db.name | quote }} }
        - { name: PGUSER, value: {{ $db.user | quote }} }
        {{- if $tls }}
        - { name: PGSSLMODE, value: verify-full }
        - { name: PGSSLROOTCERT, value: /etc/postgres-ca/ca.crt }
        {{- else }}
        - { name: PGSSLMODE, value: disable }
        {{- end }}
        - { name: KEEP, value: {{ int $b.backups.keep | quote }} }
        {{- with .file }}
        - { name: RESTORE_FILE, value: {{ . | quote }} }
        {{- end }}
      command: ["/bin/sh", "-ec"]
      args:
        - |
          # libpq ignores a password file readable by the group (0440 with
          # fsGroup): copy it to a 0600 file in the emptyDir.
          umask 077
          cp /etc/postgres-auth/pgpass /tmp/pgpass
          export PGPASSFILE=/tmp/pgpass
          # A new pod can be refused for a few seconds while the network
          # policy learns its address, and Postgres may be restarting.
          i=0; until pg_isready -q -t 5; do i=$((i + 1)); [ "$i" -lt 60 ] || { echo "postgres not reachable"; exit 1; }; sleep 2; done
          {{- .script | nindent 10 }}
      resources:
        {{- toYaml $b.backups.resources | nindent 8 }}
      volumeMounts:
        - { name: backups, mountPath: /backups }
        - { name: auth, mountPath: /etc/postgres-auth, readOnly: true }
        - { name: tmp, mountPath: /tmp }
        {{- if $tls }}
        - { name: ca, mountPath: /etc/postgres-ca, readOnly: true }
        {{- end }}
  volumes:
    - name: backups
      persistentVolumeClaim:
        claimName: {{ include "ligus-postgres.backupClaim" .ctx }}
    - name: auth
      secret:
        secretName: {{ $b.auth.existingSecret }}
        defaultMode: 0400
        items:
          - { key: {{ $b.auth.pgpassKey }}, path: pgpass }
    - name: tmp
      emptyDir: {}
    {{- if $tls }}
    - name: ca
      secret:
        secretName: {{ include "ligus-postgres.tlsSecretName" .ctx }}
        defaultMode: 0400
        items:
          - { key: {{ $b.tls.caKey }}, path: ca.crt }
    {{- end }}
{{- end -}}

{{- define "ligus-postgres.bundledBackups" -}}
{{- $db := .db -}}
{{- $b := $db.bundled -}}
{{- $name := include "ligus-postgres.backupName" . -}}
---
# pg_dump (custom format) to the backup volume, keeping the newest KEEP dumps.
# The dump is written to a temporary name, checked with pg_restore --list and
# renamed, so a failed run never replaces a good dump. Restore: README,
# "Restoring the bundled Postgres".
apiVersion: batch/v1
kind: CronJob
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres-backup
spec:
  schedule: {{ $b.backups.schedule | quote }}
  {{- with $b.backups.timeZone }}
  timeZone: {{ . | quote }}
  {{- end }}
  concurrencyPolicy: Forbid
  startingDeadlineSeconds: 3600
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  suspend: {{ if include "ligus-postgres.restoring" . }}true{{ else }}false{{ end }}
  jobTemplate:
    spec:
      backoffLimit: 2
      activeDeadlineSeconds: {{ int $b.backups.activeDeadlineSeconds }}
      template:
        {{- include "ligus-postgres.jobPod" (dict "ctx" . "container" "pg-dump" "script" (include "ligus-postgres.dumpScript" .)) | nindent 8 }}
{{- with $b.backups.restore.file }}
---
# Restore of {{ . }} (database.bundled.backups.restore.file). The app runs no
# replicas and the backup CronJob is suspended while this is set; unset it
# afterwards to bring the app back.
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ printf "%s-restore-%s" (include "ligus-postgres.bundledName" $) (. | sha256sum | trunc 8) | trunc 63 | trimSuffix "-" }}
  labels:
    {{- $.labels | nindent 4 }}
    app.kubernetes.io/component: postgres-backup
spec:
  backoffLimit: 0
  activeDeadlineSeconds: {{ int $b.backups.activeDeadlineSeconds }}
  template:
    {{- include "ligus-postgres.jobPod" (dict "ctx" $ "container" "pg-restore" "file" . "script" (include "ligus-postgres.restoreScript" $)) | nindent 4 }}
{{- end }}
{{- end -}}

{{- define "ligus-postgres.dumpScript" -}}
ts=$(date -u +%Y%m%dT%H%M%SZ)
out="/backups/${PGDATABASE}-${ts}.dump"
pg_dump --format=custom --no-owner --no-acl --file="/backups/.partial-${ts}"
pg_restore --list "/backups/.partial-${ts}" >/dev/null
mv "/backups/.partial-${ts}" "$out"
rm -f /backups/.partial-*
echo "wrote $out ($(wc -c < "$out") bytes)"
# Retention: keep the newest $KEEP dumps (names sort by time).
ls -1 /backups/"${PGDATABASE}"-*.dump | sort -r | tail -n +"$((KEEP + 1))" | while read -r old; do
  rm -f -- "$old"; echo "removed $old"
done
{{- end -}}

{{- define "ligus-postgres.restoreScript" -}}
f="/backups/${RESTORE_FILE}"
test -f "$f" || { echo "no such dump: $f"; ls -1 /backups; exit 1; }
pg_restore --list "$f" >/dev/null
# One transaction: a failure leaves the database as it was.
pg_restore --clean --if-exists --no-owner --no-acl --single-transaction --exit-on-error --dbname="$PGDATABASE" "$f"
echo "restored $f"
{{- end -}}

{{- define "ligus-postgres.bundledConf" -}}
{{- $db := .db -}}
{{- $tls := include "ligus-postgres.bundledTLS" . -}}
# First match wins. The superuser only over the local socket (peer; the
# container runs as the image's postgres user); the application role only
# to its database, with scram-sha-256{{ if $tls }}, only over TLS{{ end }}.
pg_hba.conf: |
  local     all          postgres                peer
  {{- if $tls }}
  hostssl   {{ $db.name }}  {{ $db.user }}  all  scram-sha-256
  {{- else }}
  host      {{ $db.name }}  {{ $db.user }}  all  scram-sha-256
  {{- end }}
  host      all          all          all        reject
# Runs once, on an empty data directory: the application role, not a
# superuser, owning the database. Its password is the Secret's password.
10-app-role.sh: |
  # psql reads the password file itself (never on a command line).
  psql -v ON_ERROR_STOP=1 --no-psqlrc --username postgres --dbname "$POSTGRES_DB" \
    --set=role="$LIGUS_DB_USER" --set=db="$POSTGRES_DB" <<'SQL'
  \set pw `cat /etc/postgres-secret/password`
  -- The statement below carries the password: keep it out of the server log
  -- whatever log_* parameters are set (and if it fails).
  SET log_statement = 'none';
  SET log_min_error_statement = 'panic';
  SET log_min_duration_statement = -1;
  SET password_encryption = 'scram-sha-256';
  CREATE ROLE :"role" LOGIN PASSWORD :'pw';
  ALTER DATABASE :"db" OWNER TO :"role";
  ALTER SCHEMA public OWNER TO :"role";
  REVOKE ALL ON DATABASE :"db" FROM PUBLIC;
  GRANT CONNECT, TEMPORARY ON DATABASE :"db" TO :"role";
  SQL
{{- end -}}
