{{/* Refuse database settings that cannot work or are unsafe. */}}
{{- define "langstroth-postgres.validate" -}}
{{- $db := .db -}}
{{- $root := .root -}}
{{- $p := .valuesPrefix -}}
{{- if not (has $db.mode (list "none" "external" "cnpg" "bundled")) -}}
{{- fail (printf "%s.mode must be external, cnpg, bundled or none (got %q)" $p $db.mode) -}}
{{- end -}}
{{- $skip := $db.skipCapabilityCheck -}}

{{- if eq $db.mode "external" -}}
{{- if not $db.external.secretName -}}
{{- fail (printf "%s.mode=external needs %s.external.existingSecret (a Secret holding the DSN; sslmode=verify-full with sslrootcert)" $p $p) -}}
{{- end -}}
{{- range $db.external.cidrs -}}
{{- if not (regexMatch "^[0-9a-fA-F:.]+/[0-9]+$" .) -}}
{{- fail (printf "%s.external.cidrs: %q is not a CIDR" $p .) -}}
{{- end -}}
{{- $bits := int (last (splitList "/" .)) -}}
{{- if contains ":" . -}}
{{- /* IPv6: at most a /48, and none of the forms that reach IPv4 or
     metadata addresses (IPv4-mapped/compatible ::/8, NAT64 64:ff9b::/96,
     dotted quads, the AWS IPv6 metadata fd00:ec2::254). */ -}}
{{- if or (lt $bits 48) (gt $bits 128) -}}
{{- fail (printf "%s.external.cidrs: %q is too broad for a database (IPv6 needs /48 or narrower); list its address or subnet" $p .) -}}
{{- end -}}
{{- if or (contains "." .) (regexMatch "^(::|0{1,4}:)" .) (regexMatch "(?i)^64:ff9b:" .) (regexMatch "(?i)^fd00:0?ec2:" .) -}}
{{- fail (printf "%s.external.cidrs: %q is an IPv4-mapped, NAT64 or cloud metadata IPv6 range; list the database's own address" $p .) -}}
{{- end -}}
{{- else if or (lt $bits 8) (gt $bits 32) -}}
{{- fail (printf "%s.external.cidrs: %q is too broad for a database; list its address or subnet" $p .) -}}
{{- end -}}
{{- if not (contains ":" .) -}}
{{- $c := . -}}
{{- range list "169.254.169.254" "100.100.100.200" -}}
{{- if eq (include "langstroth-postgres.cidrContains" (dict "cidr" $c "ip" .)) "1" -}}
{{- fail (printf "%s.external.cidrs: %q contains the cloud metadata address %s" $p $c .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- if eq $db.mode "cnpg" -}}
{{- $c := $db.cnpg -}}
{{- if and (not $skip) (not ($root.Capabilities.APIVersions.Has "postgresql.cnpg.io/v1/Cluster")) (not ($root.Capabilities.APIVersions.Has "postgresql.cnpg.io/v1")) -}}
{{- fail (printf "%s.mode=cnpg needs the CloudNativePG operator (API postgresql.cnpg.io/v1), which this cluster does not serve. Install it first (README, Database), or for helm template / GitOps rendering without cluster access pass --api-versions postgresql.cnpg.io/v1 or set %s.skipCapabilityCheck=true" $p $p) -}}
{{- end -}}
{{- if lt (int $c.instances) 1 -}}
{{- fail (printf "%s.cnpg.instances must be at least 1 (3 for HA)" $p) -}}
{{- end -}}
{{- if not $c.storage.size -}}
{{- fail (printf "%s.cnpg.storage.size is required" $p) -}}
{{- end -}}
{{- include "langstroth-postgres.validateBackupAck" (dict "b" $c.backups "p" (printf "%s.cnpg.backups" $p)) -}}
{{- if $c.backups.enabled -}}
{{- if not (has $c.backups.method (list "barmanObjectStore" "plugin")) -}}
{{- fail (printf "%s.cnpg.backups.method must be barmanObjectStore or plugin" $p) -}}
{{- end -}}
{{- if and (eq $c.backups.method "barmanObjectStore") (not $c.backups.barmanObjectStore.destinationPath) -}}
{{- fail (printf "%s.cnpg.backups.enabled needs somewhere to put them: set %s.cnpg.backups.barmanObjectStore (destinationPath and credentials; see the CloudNativePG docs), or method=plugin with the Barman Cloud plugin. To run without backups set %s.cnpg.backups.enabled=false and %s.cnpg.backups.acknowledgeNoBackups=true" $p $p $p $p) -}}
{{- end -}}
{{- if and (eq $c.backups.method "plugin") (or (not $c.backups.plugin.name) (not $c.backups.plugin.parameters)) -}}
{{- fail (printf "%s.cnpg.backups.method=plugin needs %s.cnpg.backups.plugin.name and parameters (e.g. barmanObjectName)" $p $p) -}}
{{- end -}}
{{- end -}}
{{- if and $c.podMonitor.enabled (not $skip) (not ($root.Capabilities.APIVersions.Has "monitoring.coreos.com/v1")) -}}
{{- fail (printf "%s.cnpg.podMonitor needs the Prometheus Operator (monitoring.coreos.com/v1); or set %s.skipCapabilityCheck=true" $p $p) -}}
{{- end -}}
{{- end -}}

{{- if eq $db.mode "bundled" -}}
{{- $b := $db.bundled -}}
{{- if not $b.auth.existingSecret -}}
{{- if $b.legacy -}}
{{- fail (printf "postgres.enabled needs postgres.existingSecret (keys password and DATABASE_URL); the chart generates no passwords, so helm template and GitOps render the same thing every time. Create it with:\n  PW=$(openssl rand -hex 24)\n  kubectl -n %s create secret generic relay-dev-postgres --from-literal=password=$PW --from-literal=DATABASE_URL=\"postgres://%s:$PW@%s:5432/%s?sslmode=disable\"\npostgres.enabled is deprecated: use %s.mode=bundled (TLS, backups)." $root.Release.Namespace $db.user (include "langstroth-postgres.bundledName" .) $db.name $p) -}}
{{- end -}}
{{- fail (printf "%s.mode=bundled needs %s.bundled.auth.existingSecret, a Secret with keys password and pgpass (the chart generates no passwords):\n  PW=$(openssl rand -hex 24)\n  kubectl -n %s create secret generic %s-auth --from-literal=password=$PW --from-literal=pgpass=\"*:*:*:%s:$PW\"" $p $p $root.Release.Namespace (include "langstroth-postgres.bundledName" .) $db.user) -}}
{{- end -}}
{{- if not $b.legacy -}}
{{- if and $b.tls.existingSecret $b.tls.certManager.issuerRef -}}
{{- fail (printf "%s.bundled.tls: set existingSecret or certManager.issuerRef, not both" $p) -}}
{{- end -}}
{{- if and (not (include "langstroth-postgres.bundledTLS" .)) (not $b.allowPlaintextInNamespace) -}}
{{- fail (printf "%s.mode=bundled needs TLS: set %s.bundled.tls.existingSecret (tls.crt, tls.key, ca.crt; SAN %s) or %s.bundled.tls.certManager.issuerRef. %s.bundled.allowPlaintextInNamespace=true accepts plaintext inside the namespace (development only)" $p $p (include "langstroth-postgres.bundledName" .) $p $p) -}}
{{- end -}}
{{- if and $b.tls.certManager.issuerRef (not $skip) (not ($root.Capabilities.APIVersions.Has "cert-manager.io/v1")) -}}
{{- fail (printf "%s.bundled.tls.certManager needs cert-manager (API cert-manager.io/v1), which this cluster does not serve; or pass --api-versions cert-manager.io/v1 / set %s.skipCapabilityCheck=true when rendering without cluster access" $p $p) -}}
{{- end -}}
{{- range $k, $_ := $b.parameters -}}
{{- if regexMatch "(?i)^(ssl|ssl_.*|hba_file|ident_file|config_file|password_encryption|listen_addresses)$" $k -}}
{{- fail (printf "%s.bundled.parameters.%s: the chart sets TLS, pg_hba, password hashing and listening itself; it can't be overridden here" $p $k) -}}
{{- end -}}
{{- end -}}
{{- if not (contains "@sha256:" (toString $b.image.tag)) -}}
{{- fail (printf "%s.bundled.image.tag must pin a digest (<tag>@sha256:...)" $p) -}}
{{- end -}}
{{- end -}}
{{- include "langstroth-postgres.validateBackupAck" (dict "b" $b.backups "p" (printf "%s.bundled.backups" $p)) -}}
{{- if and $b.backups.enabled (lt (int $b.backups.keep) 1) -}}
{{- fail (printf "%s.bundled.backups.keep must be at least 1" $p) -}}
{{- end -}}
{{- with $b.backups.restore.file -}}
{{- if not (regexMatch "^[A-Za-z0-9._-]+\\.dump$" .) -}}
{{- fail (printf "%s.bundled.backups.restore.file must be a dump file name in the backup volume (e.g. %s-20260101T031700Z.dump)" $p $db.name) -}}
{{- end -}}
{{- if not $b.backups.enabled -}}
{{- fail (printf "%s.bundled.backups.restore needs the backup volume (%s.bundled.backups.enabled=true)" $p $p) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "langstroth-postgres.validateBackupAck" -}}
{{- if and (not .b.enabled) (not .b.acknowledgeNoBackups) -}}
{{- fail (printf "%s.enabled=false: losing the volume (or a bad migration, or an operator mistake) then loses the data for good; set %s.acknowledgeNoBackups=true to accept that" .p .p) -}}
{{- end -}}
{{- end -}}
