{{/*
Defaults for every database.* key. The app charts merge their values over
these, so an upgrade with --reuse-values from a release that predates
database.* still renders. Keep in sync with the database block of both
charts' values.yaml (TestChartDatabaseDefaultsMatchValues checks it).
*/}}
{{- define "ligus-postgres.defaults" -}}
mode: ""
name: ""
user: ""
skipCapabilityCheck: false
external:
  existingSecret: ""
  existingSecretKey: DATABASE_URL
  cidrs: []
  port: 5432
cnpg:
  instances: 1
  imageName: ""
  imageCatalogRef: {}
  storage:
    size: 10Gi
    storageClass: ""
  walStorage: {}
  resources:
    requests: { cpu: 100m, memory: 256Mi }
    limits: { memory: 1Gi }
  affinity: {}
  enablePDB: null
  parameters: {}
  bootstrap: {}
  externalClusters: []
  backups:
    enabled: true
    acknowledgeNoBackups: false
    method: barmanObjectStore
    barmanObjectStore: {}
    retentionPolicy: 30d
    plugin:
      name: barman-cloud.cloudnative-pg.io
      parameters: {}
    schedule: "0 0 3 * * *"
    immediate: true
  podMonitor:
    enabled: false
    interval: ""
    labels: {}
  networkPolicy:
    enabled: true
    operatorNamespaceSelector:
      matchLabels:
        kubernetes.io/metadata.name: cnpg-system
    operatorPodSelector: {}
    metricsFrom: []
bundled:
  image:
    repository: docker.io/library/postgres
    tag: 16.16-alpine3.24@sha256:0adbf25a9e96d38823bb813f3ac0ed41fadb665c88f1df7fe6dbfb060cbdc106
    pullPolicy: IfNotPresent
  auth:
    existingSecret: ""
    pgpassKey: pgpass
  tls:
    existingSecret: ""
    caKey: ca.crt
    certManager:
      issuerRef: {}
      duration: ""
      renewBefore: ""
  allowPlaintextInNamespace: false
  persistence:
    enabled: true
    size: 5Gi
    storageClass: ""
    accessModes: [ReadWriteOncePod]
  parameters: {}
  resources:
    requests: { cpu: 50m, memory: 128Mi }
    limits: { cpu: "1", memory: 512Mi }
  nodeSelector: {}
  tolerations: []
  backups:
    enabled: true
    acknowledgeNoBackups: false
    schedule: "17 3 * * *"
    timeZone: ""
    keep: 7
    activeDeadlineSeconds: 3600
    persistence:
      size: 5Gi
      storageClass: ""
      accessModes: [ReadWriteOnce]
    resources:
      requests: { cpu: 50m, memory: 64Mi }
      limits: { memory: 512Mi }
    restore:
      file: ""
{{- end -}}
