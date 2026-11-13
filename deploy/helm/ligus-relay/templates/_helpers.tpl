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
{{- else -}}
{{- printf "%s-db" (include "ligus-relay.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "ligus-relay.databaseSecretKey" -}}
{{- if .Values.externalDatabase.existingSecret -}}
{{- .Values.externalDatabase.existingSecretKey -}}
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
{{- if and .Values.postgres.enabled (or .Values.externalDatabase.url .Values.externalDatabase.existingSecret) -}}
{{- fail "set either postgres.enabled or externalDatabase, not both" -}}
{{- end -}}
{{- if and (eq (include "ligus-relay.topology" .) "split") (not (include "ligus-relay.hasDatabase" .)) -}}
{{- fail "topology=split needs Postgres (postgres.enabled or externalDatabase): two pods cannot share the in-memory store. Use topology=single for a memory store." -}}
{{- end -}}
{{- if and .Values.persistence.enabled (include "ligus-relay.hasDatabase" .) -}}
{{- fail "persistence is the memory-store snapshot; it is not used with a database" -}}
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
      envFrom:
        - configMapRef:
            name: {{ include "ligus-relay.fullname" $root }}
        {{- with $v.config.existingSecret }}
        - secretRef:
            name: {{ . }}
        {{- end }}
      env:
        - name: LIGUS_RELAY_TUNNEL
          value: {{ .tunnelMode | quote }}
        {{- if and (ne .tunnelMode "off") (eq $v.tunnel.mode "allowlist") }}
        - name: LIGUS_RELAY_TUNNEL_HOSTS
          value: {{ join "," $v.tunnel.allowedHosts | quote }}
        {{- end }}
        {{- if include "ligus-relay.hasDatabase" $root }}
        - name: DATABASE_URL
          valueFrom:
            secretKeyRef:
              name: {{ include "ligus-relay.databaseSecretName" $root }}
              key: {{ include "ligus-relay.databaseSecretKey" $root }}
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
      livenessProbe:
        httpGet: { path: /v1/health, port: http }
        periodSeconds: 20
        timeoutSeconds: 3
        failureThreshold: 3
      resources:
        {{- toYaml .resources | nindent 8 }}
      {{- if or $v.persistence.enabled (and .withApns $v.tunnel.apns.existingSecret) }}
      volumeMounts:
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
  {{- if or $v.persistence.enabled (and .withApns $v.tunnel.apns.existingSecret) }}
  volumes:
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

{{/* In-chart Postgres password: values, else the existing Secret, else random. */}}
{{- define "ligus-relay.postgresPassword" -}}
{{- if .Values.postgres.password -}}
{{- .Values.postgres.password -}}
{{- else -}}
{{- $s := lookup "v1" "Secret" .Release.Namespace (include "ligus-relay.postgresName" .) -}}
{{- if and $s $s.data (index $s.data "password") -}}
{{- index $s.data "password" | b64dec -}}
{{- else -}}
{{- $cached := get .Values.postgres "_generated" -}}
{{- if not $cached -}}
{{- $cached = randAlphaNum 32 -}}
{{- $_ := set .Values.postgres "_generated" $cached -}}
{{- end -}}
{{- $cached -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Effective topology: "auto" is split with a database, single without. */}}
{{- define "ligus-relay.topology" -}}
{{- if eq .Values.topology "auto" -}}
{{- if include "ligus-relay.hasDatabase" . }}split{{ else }}single{{ end -}}
{{- else -}}
{{- .Values.topology -}}
{{- end -}}
{{- end -}}
