{{- define "langstroth-relay.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "langstroth-relay.fullname" -}}
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

{{- define "langstroth-relay.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "langstroth-relay.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: langstroth
{{- end -}}

{{- define "langstroth-relay.selectorLabels" -}}
app.kubernetes.io/name: {{ include "langstroth-relay.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "langstroth-relay.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "langstroth-relay.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "langstroth-relay.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
The effective database settings (YAML): database.* with the deprecated
externalDatabase.* and postgres.* keys mapped onto it when database.mode is
empty. The shape is what the langstroth-postgres library expects.
*/}}
{{- define "langstroth-relay.database" -}}
{{- $v := .Values -}}
{{- /* Chart defaults under what was set: an upgrade with --reuse-values
     carries the previous release's values, which may predate database.*. */ -}}
{{- $defaults := mergeOverwrite (include "langstroth-postgres.defaults" . | fromYaml) (dict "name" "relay" "user" "relay") -}}
{{- $d := mergeOverwrite (deepCopy $defaults) (deepCopy (default (dict) $v.database)) -}}
{{- $dep := list -}}
{{- $legacyExt := or $v.externalDatabase.url $v.externalDatabase.existingSecret -}}
{{- $_ := set $d.external "secretName" $d.external.existingSecret -}}
{{- $_ := set $d.external "secretKey" $d.external.existingSecretKey -}}
{{- $_ := set $d.bundled "legacy" false -}}
{{- if not $d.mode -}}
{{- if and $v.postgres.enabled $legacyExt -}}
{{- fail "set either postgres.enabled or externalDatabase, not both (both are deprecated: use database.mode)" -}}
{{- else if $legacyExt -}}
{{- $_ := set $d "mode" "external" -}}
{{- $_ := set $d.external "secretName" (default (printf "%s-db" (include "langstroth-relay.fullname" .)) $v.externalDatabase.existingSecret) -}}
{{- $_ := set $d.external "secretKey" (ternary $v.externalDatabase.existingSecretKey "DATABASE_URL" (not (empty $v.externalDatabase.existingSecret))) -}}
{{- $dep = append $dep "externalDatabase.* is deprecated: use database.mode=external with database.external.existingSecret (and database.external.cidrs for the egress policy)." -}}
{{- else if $v.postgres.enabled -}}
{{- $_ := set $d "mode" "bundled" -}}
{{- $_ := set $d "name" $v.postgres.database -}}
{{- $_ := set $d "user" $v.postgres.username -}}
{{- $b := $d.bundled -}}
{{- $_ := set $b "legacy" true -}}
{{- $_ := set $b "image" $v.postgres.image -}}
{{- $_ := set $b.auth "existingSecret" $v.postgres.existingSecret -}}
{{- $_ := set $b "tls" (dict "existingSecret" "" "caKey" "ca.crt" "certManager" (dict "issuerRef" (dict))) -}}
{{- $_ := set $b "allowPlaintextInNamespace" true -}}
{{- $_ := set $b "persistence" (merge (dict "accessModes" (list "ReadWriteOnce")) $v.postgres.persistence) -}}
{{- $_ := set $b "resources" $v.postgres.resources -}}
{{- $_ := set $b "parameters" (dict) -}}
{{- $_ := set $b "backups" (merge (dict "enabled" false "acknowledgeNoBackups" true "restore" (dict "file" "")) (omit $b.backups "enabled" "acknowledgeNoBackups" "restore")) -}}
{{- $dep = append $dep "postgres.enabled is deprecated: it still runs the old development pod (no TLS, no backups, superuser role). Use database.mode=bundled (TLS, pg_dump backups) or database.mode=cnpg; see the README for moving the data." -}}
{{- else -}}
{{- $_ := set $d "mode" "none" -}}
{{- end -}}
{{- else if or $legacyExt $v.postgres.enabled -}}
{{- fail "database.mode is set: remove the deprecated externalDatabase.* / postgres.enabled settings (they would be ignored)" -}}
{{- end -}}
{{- $_ := set $d "deprecated" $dep -}}
{{- toYaml $d -}}
{{- end -}}

{{/* The context dict for the langstroth-postgres library (call with the root). */}}
{{- define "langstroth-relay.dbCtx" -}}
{{- $sel := dict "matchLabels" (dict "app.kubernetes.io/name" (include "langstroth-relay.name" .) "app.kubernetes.io/instance" .Release.Name) "matchExpressions" (list (dict "key" "app.kubernetes.io/component" "operator" "In" "values" (list "relay" "sync" "tunnel"))) -}}
{{- toYaml (dict
  "db" (include "langstroth-relay.database" . | fromYaml)
  "fullname" (include "langstroth-relay.fullname" .)
  "labels" (include "langstroth-relay.labels" .)
  "dbNameLabel" (include "langstroth-relay.name" .)
  "clientSelector" $sel
  "mountBase" "/etc/langstroth-relay"
  "dsnFile" "DATABASE_URL"
  "valuesPrefix" "database"
  "networkPolicy" .Values.networkPolicy.enabled
  "restrictEgress" .Values.networkPolicy.restrictEgress
  "dns" .Values.networkPolicy.dns) -}}
{{- end -}}

{{/* true when the relay gets a DATABASE_URL */}}
{{- define "langstroth-relay.hasDatabase" -}}
{{- if ne (include "langstroth-relay.database" . | fromYaml).mode "none" -}}true{{- end -}}
{{- end -}}

{{/* Fail early on combinations that cannot work. */}}
{{- define "langstroth-relay.validate" -}}
{{- if not (has (include "langstroth-relay.topology" .) (list "split" "single")) -}}
{{- fail "topology must be split or single" -}}
{{- end -}}
{{- if not (has .Values.tunnel.mode (list "off" "open" "allowlist")) -}}
{{- fail "tunnel.mode must be off, open or allowlist" -}}
{{- end -}}
{{- if and (eq .Values.tunnel.mode "allowlist") (empty .Values.tunnel.allowedHosts) -}}
{{- fail "tunnel.mode=allowlist needs tunnel.allowedHosts" -}}
{{- end -}}
{{- if and (eq (include "langstroth-relay.topology" .) "split") (not (include "langstroth-relay.hasDatabase" .)) -}}
{{- fail "topology=split needs Postgres (database.mode external, cnpg or bundled): two pods cannot share the in-memory store. Use topology=single for a memory store." -}}
{{- end -}}
{{- if and .Values.persistence.enabled (include "langstroth-relay.hasDatabase" .) -}}
{{- fail "persistence is the memory-store snapshot; it is not used with a database" -}}
{{- end -}}
{{- if and .Values.tunnel.apns.existingSecret (eq .Values.tunnel.apns.existingSecret .Values.config.existingSecret) -}}
{{- fail "tunnel.apns.existingSecret must not be config.existingSecret: that Secret is loaded into every pod, relay-sync included. Put the APNs key in a Secret of its own." -}}
{{- end -}}
{{- range $k, $_ := .Values.config.env -}}
{{- if regexMatch "^(LIGUS|HIVE)_RELAY_" $k -}}
{{- fail (printf "config.env.%s: HIVE_RELAY_* and LIGUS_RELAY_* names are no longer read (the relay refuses to start with one set); use LANGSTROTH_RELAY_%s" $k (regexReplaceAll "^(LIGUS|HIVE)_RELAY_" $k "")) -}}
{{- end -}}
{{- if regexMatch "^LANGSTROTH_RELAY_TENANCY" $k -}}
{{- fail (printf "config.env.%s: the tenancy config is a file; use tenancy.existingSecret" $k) -}}
{{- end -}}
{{- if regexMatch "^LANGSTROTH_RELAY_APNS_" $k -}}
{{- fail (printf "config.env.%s: APNs settings reach every pod through config.env; use tunnel.apns.* (the key from tunnel.apns.existingSecret)" $k) -}}
{{- end -}}
{{- end -}}
{{- if or (gt (int .Values.sync.replicas) 1) .Values.sync.autoscaling.enabled -}}
{{- fail "relay-sync runs exactly one replica in 2.0: short pairing codes, SSE fan-out and rate limits live in one process, so a second replica breaks pairing (README, Scaling). Set sync.replicas=1 and sync.autoscaling.enabled=false" -}}
{{- end -}}
{{- /* A LoadBalancer or NodePort Service publishes the relay like an Ingress does. */ -}}
{{- $public := or .Values.ingress.enabled .Values.httpRoute.enabled (ne (toString .Values.service.type) "ClusterIP") -}}
{{- if and .Values.ingress.enabled (not .Values.ingress.tls.secretName) (not .Values.ingress.tls.terminatedUpstream) -}}
{{- fail "ingress.enabled needs TLS: clients require https://. Set ingress.tls.secretName, or ingress.tls.terminatedUpstream=true when a load balancer in front terminates TLS" -}}
{{- end -}}
{{- if and $public (not .Values.clientIP.trustedProxies) (not .Values.clientIP.allowSharedBucket) -}}
{{- fail "behind an ingress, gateway or a LoadBalancer/NodePort Service set clientIP.trustedProxies to the exact addresses it connects from (never the whole pod CIDR); without it every client shares one rate-limit bucket. clientIP.allowSharedBucket=true accepts that" -}}
{{- end -}}
{{- if and $public (not .Values.metrics.token.existingSecret) (not .Values.metrics.allowPublicUnauthenticated) -}}
{{- fail "with ingress, httpRoute or a LoadBalancer/NodePort service.type /metrics would be public: set metrics.token.existingSecret (LANGSTROTH_RELAY_METRICS_TOKEN), or metrics.allowPublicUnauthenticated=true" -}}
{{- end -}}
{{- if .Values.sync.pdb.enabled -}}
{{- fail "sync.pdb: a PodDisruptionBudget on a single relay-sync replica only blocks node drains; relay-sync HA is unsupported in 2.0" -}}
{{- end -}}
{{- if and .Values.persistence.enabled (eq (include "langstroth-relay.topology" .) "split") -}}
{{- fail "persistence (memory-store snapshot) only works with topology=single" -}}
{{- end -}}
{{- end -}}

{{/*
Pod template shared by the sync, tunnel and single Deployments.
Args: dict root component tunnelMode resources withApns
*/}}
{{- define "langstroth-relay.podTemplate" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
metadata:
  labels:
    {{- include "langstroth-relay.selectorLabels" (dict "root" $root "component" .component) | nindent 4 }}
    {{- with $v.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  annotations:
    checksum/config: {{ include (print $root.Template.BasePath "/configmap.yaml") $root | sha256sum }}
    checksum/secrets: {{ include "langstroth-relay.secretsChecksum" (dict "root" $root "withApns" .withApns) }}
    {{- with $v.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  serviceAccountName: {{ include "langstroth-relay.serviceAccountName" $root }}
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
      image: {{ include "langstroth-relay.image" $root }}
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
      # directory the relay reads by setting name, LANGSTROTH_RELAY_SECRETS_DIR).
      envFrom:
        - configMapRef:
            name: {{ include "langstroth-relay.fullname" $root }}
      env:
        - name: LANGSTROTH_RELAY_TUNNEL
          value: {{ .tunnelMode | quote }}
        {{- if eq .component "tunnel" }}
        # Only /v1/tunnel/*, /v1/health and /metrics here: pairing and the
        # rest belong to relay-sync's single store.
        - name: LANGSTROTH_RELAY_ROUTES
          value: tunnel
        {{- end }}
        {{- if and (ne .tunnelMode "off") (eq $v.tunnel.mode "allowlist") }}
        - name: LANGSTROTH_RELAY_TUNNEL_HOSTS
          value: {{ join "," $v.tunnel.allowedHosts | quote }}
        {{- end }}
        {{- with $v.config.existingSecret }}
        - name: LANGSTROTH_RELAY_SECRETS_DIR
          value: /etc/langstroth-relay/secrets
        {{- end }}
        {{- if $v.metrics.token.existingSecret }}
        - name: LANGSTROTH_RELAY_METRICS_TOKEN_FILE
          value: /etc/langstroth-relay/metrics/token
        {{- end }}
        {{- if include "langstroth-relay.hasDatabase" $root }}
        - name: DATABASE_URL_FILE
          value: /etc/langstroth-relay/db/DATABASE_URL
        {{- end }}
        {{- if $v.tenancy.existingSecret }}
        - name: LANGSTROTH_RELAY_TENANCY_FILE
          value: /etc/langstroth-relay/tenancy/tenancy.json
        {{- end }}
        {{- if $v.persistence.enabled }}
        - name: LANGSTROTH_RELAY_DATA_DIR
          value: {{ $v.persistence.mountPath | quote }}
        {{- end }}
        {{- if and .withApns $v.tunnel.apns.existingSecret }}
        - name: LANGSTROTH_RELAY_APNS_KEY_FILE
          value: /etc/langstroth-relay/apns/{{ $v.tunnel.apns.keyFileKey }}
        - name: LANGSTROTH_RELAY_APNS_KEY_ID
          value: {{ $v.tunnel.apns.keyId | quote }}
        - name: LANGSTROTH_RELAY_APNS_TEAM_ID
          value: {{ $v.tunnel.apns.teamId | quote }}
        - name: LANGSTROTH_RELAY_APNS_TOPIC
          value: {{ $v.tunnel.apns.topic | quote }}
        {{- with $v.tunnel.apns.topics }}
        - name: LANGSTROTH_RELAY_APNS_TOPICS
          value: {{ join "," . | quote }}
        {{- end }}
        - name: LANGSTROTH_RELAY_APNS_ENV
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
      {{- if or $v.persistence.enabled (and .withApns $v.tunnel.apns.existingSecret) $v.config.existingSecret (include "langstroth-relay.hasDatabase" $root) $v.metrics.token.existingSecret $v.tenancy.existingSecret }}
      volumeMounts:
        {{- if $v.tenancy.existingSecret }}
        - name: tenancy
          mountPath: /etc/langstroth-relay/tenancy
          readOnly: true
        {{- end }}
        {{- if $v.metrics.token.existingSecret }}
        - name: metrics-token
          mountPath: /etc/langstroth-relay/metrics
          readOnly: true
        {{- end }}
        {{- if $v.config.existingSecret }}
        - name: secrets
          mountPath: /etc/langstroth-relay/secrets
          readOnly: true
        {{- end }}
        {{- if include "langstroth-relay.hasDatabase" $root }}
        - name: db
          mountPath: /etc/langstroth-relay/db
          readOnly: true
        {{- end }}
        {{- if $v.persistence.enabled }}
        - name: data
          mountPath: {{ $v.persistence.mountPath }}
        {{- end }}
        {{- if and .withApns $v.tunnel.apns.existingSecret }}
        - name: apns
          mountPath: /etc/langstroth-relay/apns
          readOnly: true
        {{- end }}
      {{- end }}
  {{- if or $v.persistence.enabled (and .withApns $v.tunnel.apns.existingSecret) $v.config.existingSecret (include "langstroth-relay.hasDatabase" $root) $v.metrics.token.existingSecret $v.tenancy.existingSecret }}
  volumes:
    {{- with $v.tenancy.existingSecret }}
    - name: tenancy
      secret:
        secretName: {{ . }}
        defaultMode: 0400
        items:
          - { key: {{ $v.tenancy.key }}, path: tenancy.json }
    {{- end }}
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
    {{- if include "langstroth-relay.hasDatabase" $root }}
    {{- $ctx := include "langstroth-relay.dbCtx" $root | fromYaml }}
    {{- $_ := set $ctx "root" $root }}
    {{- include "langstroth-postgres.clientVolume" $ctx | nindent 4 }}
    {{- end }}
    {{- if $v.persistence.enabled }}
    - name: data
      persistentVolumeClaim:
        claimName: {{ include "langstroth-relay.fullname" $root }}-data
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
{{- define "langstroth-relay.topology" -}}
{{- if eq .Values.topology "auto" -}}
{{- if include "langstroth-relay.hasDatabase" . }}split{{ else }}single{{ end -}}
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
{{- define "langstroth-relay.secretsChecksum" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
{{- $names := list -}}
{{- with $v.config.existingSecret }}{{ $names = append $names . }}{{ end -}}
{{- with $v.metrics.token.existingSecret }}{{ $names = append $names . }}{{ end -}}
{{- with $v.tenancy.existingSecret }}{{ $names = append $names . }}{{ end -}}
{{- $ctx := include "langstroth-relay.dbCtx" $root | fromYaml -}}
{{- $_ := set $ctx "root" $root -}}
{{- range (include "langstroth-postgres.clientSecretNames" $ctx | fromJsonArray) }}{{ $names = append $names . }}{{ end -}}
{{- if and .withApns $v.tunnel.apns.existingSecret }}{{ $names = append $names $v.tunnel.apns.existingSecret }}{{ end -}}
{{- $data := dict "rendered" (include (print $root.Template.BasePath "/secret-db.yaml") $root) -}}
{{- if include "langstroth-postgres.builtDSN" $ctx }}{{ $_ := set $data "dsn" (include "langstroth-postgres.dsn" $ctx) }}{{ end -}}
{{- range $n := ($names | uniq | sortAlpha) -}}
{{- $s := lookup "v1" "Secret" $root.Release.Namespace $n -}}
{{- $_ := set $data $n (dig "data" (dict) $s) -}}
{{- end -}}
{{- toJson $data | sha256sum -}}
{{- end -}}
