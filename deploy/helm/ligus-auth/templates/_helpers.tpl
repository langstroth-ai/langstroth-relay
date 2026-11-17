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
{{- if eq .Values.store.driver "postgres" -}}
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
{{- if eq .Values.store.driver "postgres" }}{{ $names = append $names .Values.store.databaseUrl.existingSecret }}{{ end -}}
{{- $data := dict -}}
{{- range $n := ($names | compact | uniq | sortAlpha) -}}
{{- $s := lookup "v1" "Secret" $.Release.Namespace $n -}}
{{- $_ := set $data $n (dig "data" (dict) $s) -}}
{{- end -}}
{{- toJson $data | sha256sum -}}
{{- end -}}
