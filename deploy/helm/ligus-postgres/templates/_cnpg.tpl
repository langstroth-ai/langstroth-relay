{{/*
database.mode=cnpg: a CloudNativePG Cluster in the release namespace. The
operator (a prerequisite) generates the app user's Secret (<name>-app) and
the server CA (<name>-ca); the client mounts only pgpass and ca.crt from
them and connects with sslmode=verify-full. Non-TLS connections are refused
by pg_hba.
*/}}
{{- define "ligus-postgres.cnpg" -}}
{{- $db := .db -}}
{{- $c := $db.cnpg -}}
{{- $name := include "ligus-postgres.cnpgName" . }}
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
spec:
  instances: {{ int $c.instances }}
  {{- with $c.imageName }}
  imageName: {{ . }}
  {{- end }}
  {{- with $c.imageCatalogRef }}
  imageCatalogRef:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  primaryUpdateStrategy: unsupervised
  storage:
    size: {{ $c.storage.size }}
    {{- with $c.storage.storageClass }}
    storageClass: {{ . }}
    {{- end }}
  {{- with $c.walStorage }}
  walStorage:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $c.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $c.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- if not (kindIs "invalid" $c.enablePDB) }}
  enablePDB: {{ $c.enablePDB }}
  {{- end }}
  bootstrap:
    {{- if $c.bootstrap }}
    {{- toYaml $c.bootstrap | nindent 4 }}
    {{- else }}
    initdb:
      database: {{ $db.name }}
      owner: {{ $db.user }}
    {{- end }}
  {{- with $c.externalClusters }}
  externalClusters:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  postgresql:
    {{- with $c.parameters }}
    parameters:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    # TLS only for applications: the operator's own rules (local peer,
    # streaming_replica by certificate) come first; then refuse anything
    # without TLS, before the default scram-sha-256 rule.
    pg_hba:
      - hostnossl all all all reject
  {{- if $c.backups.enabled }}
  {{- if eq $c.backups.method "barmanObjectStore" }}
  backup:
    {{- with $c.backups.retentionPolicy }}
    retentionPolicy: {{ . | quote }}
    {{- end }}
    barmanObjectStore:
      {{- toYaml $c.backups.barmanObjectStore | nindent 6 }}
  {{- else }}
  plugins:
    - name: {{ $c.backups.plugin.name }}
      isWALArchiver: true
      parameters:
        {{- toYaml $c.backups.plugin.parameters | nindent 8 }}
  {{- end }}
---
apiVersion: postgresql.cnpg.io/v1
kind: ScheduledBackup
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
spec:
  # Six fields (seconds first), CloudNativePG's cron format.
  schedule: {{ $c.backups.schedule | quote }}
  immediate: {{ $c.backups.immediate }}
  backupOwnerReference: self
  cluster:
    name: {{ $name }}
  {{- if eq $c.backups.method "plugin" }}
  method: plugin
  pluginConfiguration:
    name: {{ $c.backups.plugin.name }}
  {{- else }}
  method: barmanObjectStore
  {{- end }}
  {{- end }}
{{- if $c.podMonitor.enabled }}
---
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: {{ $name }}
  labels:
    {{- .labels | nindent 4 }}
    app.kubernetes.io/component: postgres
    {{- with $c.podMonitor.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  selector:
    matchLabels:
      cnpg.io/cluster: {{ $name }}
      cnpg.io/podRole: instance
  podMetricsEndpoints:
    - port: metrics
      {{- with $c.podMonitor.interval }}
      interval: {{ . }}
      {{- end }}
{{- end }}
{{- if and .networkPolicy $c.networkPolicy.enabled }}
---
# Only the clients, the cluster's own instances and the operator reach the
# instances; metrics only from metricsFrom.
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
      cnpg.io/cluster: {{ $name }}
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            {{- toYaml .clientSelector | nindent 12 }}
      ports:
        - { port: 5432, protocol: TCP }
    - from:
        - podSelector:
            matchLabels:
              cnpg.io/cluster: {{ $name }}
      ports:
        - { port: 5432, protocol: TCP }
        - { port: 8000, protocol: TCP }
    - from:
        - namespaceSelector:
            {{- toYaml $c.networkPolicy.operatorNamespaceSelector | nindent 12 }}
          {{- with $c.networkPolicy.operatorPodSelector }}
          podSelector:
            {{- toYaml . | nindent 12 }}
          {{- end }}
      ports:
        - { port: 5432, protocol: TCP }
        - { port: 8000, protocol: TCP }
    {{- with $c.networkPolicy.metricsFrom }}
    - from:
        {{- toYaml . | nindent 8 }}
      ports:
        - { port: 9187, protocol: TCP }
    {{- end }}
{{- end }}
{{- end -}}
