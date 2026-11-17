{{- define "ligus-relay.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "ligus-relay.fullname" -}}
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

{{- define "ligus-relay.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "ligus-relay.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ligus
{{- end -}}

{{- define "ligus-relay.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ligus-relay.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "ligus-relay.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ligus-relay.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "ligus-relay.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/* true when the relay gets a DATABASE_URL */}}
{{- define "ligus-relay.hasDatabase" -}}
{{- if or .Values.postgres.enabled .Values.externalDatabase.url .Values.externalDatabase.existingSecret -}}true{{- end -}}
{{- end -}}

{{- define "ligus-relay.databaseSecretName" -}}
{{- if .Values.externalDatabase.existingSecret -}}
{{- .Values.externalDatabase.existingSecret -}}
{{- else if .Values.postgres.enabled -}}
{{- .Values.postgres.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "ligus-relay.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "ligus-relay.databaseSecretKey" -}}
{{- if .Values.externalDatabase.existingSecret -}}
{{- .Values.externalDatabase.existingSecretKey -}}
{{- else if .Values.postgres.enabled -}}
DATABASE_URL
{{- else -}}
DATABASE_URL
{{- end -}}
{{- end -}}

{{- define "ligus-relay.postgresName" -}}
{{- printf "%s-postgres" (include "ligus-relay.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fail early on combinations that cannot work. */}}
{{- define "ligus-relay.validate" -}}
{{- if not (has (include "ligus-relay.topology" .) (list "split" "single")) -}}
{{- fail "topology must be split or single" -}}
{{- end -}}
{{- if not (has .Values.tunnel.mode (list "off" "open" "allowlist")) -}}
{{- fail "tunnel.mode must be off, open or allowlist" -}}
{{- end -}}
{{- if and (eq .Values.tunnel.mode "allowlist") (empty .Values.tunnel.allowedHosts) -}}
{{- fail "tunnel.mode=allowlist needs tunnel.allowedHosts" -}}
{{- end -}}
{{- if and .Values.postgres.enabled (not .Values.postgres.existingSecret) -}}
{{- fail (printf "postgres.enabled needs postgres.existingSecret (keys password and DATABASE_URL); the chart generates no passwords, so helm template and GitOps render the same thing every time. Create it with:\n  PW=$(openssl rand -hex 24)\n  kubectl -n %s create secret generic relay-dev-postgres --from-literal=password=$PW --from-literal=DATABASE_URL=\"postgres://%s:$PW@%s:5432/%s?sslmode=disable\"" .Release.Namespace .Values.postgres.username (include "ligus-relay.postgresName" .) .Values.postgres.database) -}}
{{- end -}}
{{- if and .Values.postgres.enabled (or .Values.externalDatabase.url .Values.externalDatabase.existingSecret) -}}
{{- fail "set either postgres.enabled or externalDatabase, not both" -}}
{{- end -}}
{{- if and (eq (include "ligus-relay.topology" .) "split") (not (include "ligus-relay.hasDatabase" .)) -}}
{{- fail "topology=split needs Postgres (postgres.enabled or externalDatabase): two pods cannot share the in-memory store. Use topology=single for a memory store." -}}
{{- end -}}
{{- if and .Values.persistence.enabled (include "ligus-relay.hasDatabase" .) -}}
{{- fail "persistence is the memory-store snapshot; it is not used with a database" -}}
{{- end -}}
{{- if and .Values.tunnel.apns.existingSecret (eq .Values.tunnel.apns.existingSecret .Values.config.existingSecret) -}}
{{- fail "tunnel.apns.existingSecret must not be config.existingSecret: that Secret is loaded into every pod, relay-sync included. Put the APNs key in a Secret of its own." -}}
{{- end -}}
{{- range $k, $_ := .Values.config.env -}}
{{- if regexMatch "^(LIGUS|HIVE)_RELAY_APNS_" $k -}}
{{- fail (printf "config.env.%s: APNs settings reach every pod through config.env; use tunnel.apns.* (the key from tunnel.apns.existingSecret)" $k) -}}
{{- end -}}
{{- end -}}
{{- if or (gt (int .Values.sync.replicas) 1) .Values.sync.autoscaling.enabled -}}
{{- fail "relay-sync runs exactly one replica in 2.0: short pairing codes, SSE fan-out and rate limits live in one process, so a second replica breaks pairing (README, Scaling). Set sync.replicas=1 and sync.autoscaling.enabled=false" -}}
{{- end -}}
{{- $public := or .Values.ingress.enabled .Values.httpRoute.enabled -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.tls.secretName) (not .Values.ingress.tls.terminatedUpstream) -}}
{{- fail "ingress.enabled needs TLS: clients require https://. Set ingress.tls.secretName, or ingress.tls.terminatedUpstream=true when a load balancer in front terminates TLS" -}}
{{- end -}}
{{- if and $public (not .Values.clientIP.trustedProxies) (not .Values.clientIP.allowSharedBucket) -}}
{{- fail "behind an ingress or gateway set clientIP.trustedProxies to the exact addresses it connects from (never the whole pod CIDR); without it every client shares one rate-limit bucket. clientIP.allowSharedBucket=true accepts that" -}}
{{- end -}}
{{- if and $public (not .Values.metrics.token.existingSecret) (not .Values.metrics.allowPublicUnauthenticated) -}}
{{- fail "with ingress or httpRoute /metrics would be public: set metrics.token.existingSecret (LIGUS_RELAY_METRICS_TOKEN), or metrics.allowPublicUnauthenticated=true" -}}
{{- end -}}
{{- if .Values.sync.pdb.enabled -}}
{{- fail "sync.pdb: a PodDisruptionBudget on a single relay-sync replica only blocks node drains; relay-sync HA is unsupported in 2.0" -}}
{{- end -}}
{{- if and .Values.persistence.enabled (eq (include "ligus-relay.topology" .) "split") -}}
{{- fail "persistence (memory-store snapshot) only works with topology=single" -}}
{{- end -}}
{{- end -}}

{{/*
Pod template shared by the sync, tunnel and single Deployments.
Args: dict root component tunnelMode resources withApns
*/}}
{{- define "ligus-relay.podTemplate" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
metadata:
  labels:
    {{- include "ligus-relay.selectorLabels" (dict "root" $root "component" .component) | nindent 4 }}
    {{- with $v.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  annotations:
    checksum/config: {{ include (print $root.Template.BasePath "/configmap.yaml") $root | sha256sum }}
    checksum/secrets: {{ include "ligus-relay.secretsChecksum" (dict "root" $root "withApns" .withApns) }}
    {{- with $v.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  serviceAccountName: {{ include "ligus-relay.serviceAccountName" $root }}
  automountServiceAccountToken: false
  enableServiceLinks: false
  {{- with $v.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  securityContext:
    {{- toYaml $v.podSecurityContext | nindent 4 }}
  terminationGracePeriodSeconds: {{ $v.terminationGracePeriodSeconds }}
  containers:
    - name: relay
      image: {{ include "ligus-relay.image" $root }}
      imagePullPolicy: {{ $v.image.pullPolicy }}
      {{- with $v.image.command }}
      command:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml $v.securityContext | nindent 8 }}
      ports:
        - name: http
          containerPort: 8443
          protocol: TCP
      # Settings only; secrets are files (config.existingSecret is a
      # directory the relay reads by setting name, LIGUS_RELAY_SECRETS_DIR).
      envFrom:
        - configMapRef:
            name: {{ include "ligus-relay.fullname" $root }}
      env:
        - name: LIGUS_RELAY_TUNNEL
          value: {{ .tunnelMode | quote }}
        {{- if and (ne .tunnelMode "off") (eq $v.tunnel.mode "allowlist") }}
        - name: LIGUS_RELAY_TUNNEL_HOSTS
          value: {{ join "," $v.tunnel.allowedHosts | quote }}
        {{- end }}
        {{- with $v.config.existingSecret }}
        - name: LIGUS_RELAY_SECRETS_DIR
          value: /etc/ligus-relay/secrets
        {{- end }}
        {{- if $v.metrics.token.existingSecret }}
        - name: LIGUS_RELAY_METRICS_TOKEN_FILE
          value: /etc/ligus-relay/metrics/token
        {{- end }}
        {{- if include "ligus-relay.hasDatabase" $root }}
        - name: DATABASE_URL_FILE
          value: /etc/ligus-relay/db/DATABASE_URL
        {{- end }}
        {{- if $v.persistence.enabled }}
        - name: LIGUS_RELAY_DATA_DIR
          value: {{ $v.persistence.mountPath | quote }}
        {{- end }}
        {{- if and .withApns $v.tunnel.apns.existingSecret }}
        - name: LIGUS_RELAY_APNS_KEY_FILE
          value: /etc/ligus-relay/apns/{{ $v.tunnel.apns.keyFileKey }}
        - name: LIGUS_RELAY_APNS_KEY_ID
          value: {{ $v.tunnel.apns.keyId | quote }}
        - name: LIGUS_RELAY_APNS_TEAM_ID
          value: {{ $v.tunnel.apns.teamId | quote }}
        - name: LIGUS_RELAY_APNS_TOPIC
          value: {{ $v.tunnel.apns.topic | quote }}
        {{- with $v.tunnel.apns.topics }}
        - name: LIGUS_RELAY_APNS_TOPICS
          value: {{ join "," . | quote }}
        {{- end }}
        - name: LIGUS_RELAY_APNS_ENV
          value: {{ $v.tunnel.apns.env | quote }}
        {{- end }}
      startupProbe:
        httpGet: { path: /v1/health, port: http }
        periodSeconds: 2
        failureThreshold: 30
      readinessProbe:
        httpGet: { path: /v1/health, port: http }
        periodSeconds: 10
        timeoutSeconds: 3
      # Liveness checks only that the process accepts connections:
      # /v1/health round-trips the store, and restarting every pod because the
      # database is down fixes nothing (and drops every tunnel socket).
      livenessProbe:
        tcpSocket: { port: http }
        periodSeconds: 20
        timeoutSeconds: 3
        failureThreshold: 3
      resources:
        {{- toYaml .resources | nindent 8 }}
      {{- if or $v.persistence.enabled (and .withApns $v.tunnel.apns.existingSecret) $v.config.existingSecret (include "ligus-relay.hasDatabase" $root) $v.metrics.token.existingSecret }}
      volumeMounts:
        {{- if $v.metrics.token.existingSecret }}
        - name: metrics-token
          mountPath: /etc/ligus-relay/metrics
          readOnly: true
        {{- end }}
        {{- if $v.config.existingSecret }}
        - name: secrets
          mountPath: /etc/ligus-relay/secrets
          readOnly: true
        {{- end }}
        {{- if include "ligus-relay.hasDatabase" $root }}
        - name: db
          mountPath: /etc/ligus-relay/db
          readOnly: true
        {{- end }}
        {{- if $v.persistence.enabled }}
        - name: data
          mountPath: {{ $v.persistence.mountPath }}
        {{- end }}
        {{- if and .withApns $v.tunnel.apns.existingSecret }}
        - name: apns
          mountPath: /etc/ligus-relay/apns
          readOnly: true
        {{- end }}
      {{- end }}
  {{- if or $v.persistence.enabled (and .withApns $v.tunnel.apns.existingSecret) $v.config.existingSecret (include "ligus-relay.hasDatabase" $root) $v.metrics.token.existingSecret }}
  volumes:
    {{- with $v.metrics.token.existingSecret }}
    - name: metrics-token
      secret:
        secretName: {{ . }}
        defaultMode: 0400
        items:
          - { key: {{ $v.metrics.token.key }}, path: token }
    {{- end }}
    # 0400: with fsGroup the kubelet adds group read (0440, group 65532).
    {{- with $v.config.existingSecret }}
    - name: secrets
      secret:
        secretName: {{ . }}
        defaultMode: 0400
    {{- end }}
    {{- if include "ligus-relay.hasDatabase" $root }}
    - name: db
      secret:
        secretName: {{ include "ligus-relay.databaseSecretName" $root }}
        defaultMode: 0400
        items:
          - { key: {{ include "ligus-relay.databaseSecretKey" $root }}, path: DATABASE_URL }
    {{- end }}
    {{- if $v.persistence.enabled }}
    - name: data
      persistentVolumeClaim:
        claimName: {{ include "ligus-relay.fullname" $root }}-data
    {{- end }}
    {{- if and .withApns $v.tunnel.apns.existingSecret }}
    - name: apns
      secret:
        secretName: {{ $v.tunnel.apns.existingSecret }}
        defaultMode: 0400
    {{- end }}
  {{- end }}
  {{- with $v.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with $v.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}

{{/* Effective topology: "auto" is split with a database, single without. */}}
{{- define "ligus-relay.topology" -}}
{{- if eq .Values.topology "auto" -}}
{{- if include "ligus-relay.hasDatabase" . }}split{{ else }}single{{ end -}}
{{- else -}}
{{- .Values.topology -}}
{{- end -}}
{{- end -}}

{{/*
checksum/secrets: rolls pods when a Secret they read changes. Chart-rendered
Secrets are hashed as rendered; existing Secrets through lookup, which is a
constant under `helm template` (with Argo CD or Flux use Reloader, or bump
podAnnotations). Args: dict root withApns.
*/}}
{{- define "ligus-relay.secretsChecksum" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
{{- $names := list -}}
{{- with $v.config.existingSecret }}{{ $names = append $names . }}{{ end -}}
{{- with $v.metrics.token.existingSecret }}{{ $names = append $names . }}{{ end -}}
{{- if include "ligus-relay.hasDatabase" $root }}{{ $names = append $names (include "ligus-relay.databaseSecretName" $root) }}{{ end -}}
{{- if and .withApns $v.tunnel.apns.existingSecret }}{{ $names = append $names $v.tunnel.apns.existingSecret }}{{ end -}}
{{- $data := dict "rendered" (include (print $root.Template.BasePath "/secret-db.yaml") $root) -}}
{{- range $n := ($names | uniq | sortAlpha) -}}
{{- $s := lookup "v1" "Secret" $root.Release.Namespace $n -}}
{{- $_ := set $data $n (dig "data" (dict) $s) -}}
{{- end -}}
{{- toJson $data | sha256sum -}}
{{- end -}}
