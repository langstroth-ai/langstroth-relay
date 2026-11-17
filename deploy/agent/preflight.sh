#!/usr/bin/env bash
# Preflight for installing the ligus-relay / ligus-auth charts. Read-only:
# it changes nothing, unless --probe is given, which creates one throwaway
# namespace (ligus-preflight-<random>), tests NetworkPolicy enforcement and
# ReadWriteOncePod there, and deletes it.
#
#   deploy/agent/preflight.sh --context orbstack --namespace ligus-relay \
#     --scenario team-bundled [--probe] [--json]
#
# Exit: 0 every check passed or warned, 1 a check failed, 2 usage,
# 3 kubectl/helm missing or the cluster unreachable.
source "$(dirname "$0")/lib.sh"

usage() {
  cat >&2 <<EOF
usage: $0 [--context CTX] [--namespace NS] [--scenario S] [--probe] [--json]
  --context    kube context to check (default: the current one; always named in the output)
  --namespace  target namespace (default: ligus-relay)
  --scenario   one of: $SCENARIOS
  --probe      also prove NetworkPolicy enforcement and RWOP with a throwaway namespace
  --json       one JSON object on stdout
EOF
}

NS=${LIGUS_NAMESPACE:-ligus-relay}
SCENARIO=${LIGUS_SCENARIO:-}
PROBE=0
while [ $# -gt 0 ]; do
  case $1 in
    --context) KCTX=${2:?}; shift 2 ;;
    --namespace) NS=${2:?}; shift 2 ;;
    --scenario) SCENARIO=${2:?}; shift 2 ;;
    --probe) PROBE=1; shift ;;
    --json) JSON=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die_usage "unknown argument $1" ;;
  esac
done
if [ -n "$SCENARIO" ] && ! valid_scenario "$SCENARIO"; then die_usage "unknown scenario $SCENARIO (one of: $SCENARIOS)"; fi

need_tool kubectl "install kubectl >= 1.27"
need_tool helm "install Helm 3.14+ (Helm 4 works)"

emit() {
  if [ "$JSON" = 1 ]; then
    local ok=true; [ "$FAILED" = 1 ] && ok=false
    printf '{"tool":"preflight","ok":%s,"context":%s,"namespace":%s,"scenario":%s,"probe":%s,"checks":%s}\n' \
      "$ok" "$(json_escape "$KCTX")" "$(json_escape "$NS")" "$(json_escape "$SCENARIO")" \
      "$([ "$PROBE" = 1 ] && echo true || echo false)" "$(checks_json)"
  fi
}

# --- Helm -------------------------------------------------------------------
hv=$(helm version --template '{{.Version}}' 2>/dev/null || true)
hmaj=$(printf '%s' "$hv" | sed -E 's/^v([0-9]+)\.([0-9]+).*/\1/')
hmin=$(printf '%s' "$hv" | sed -E 's/^v([0-9]+)\.([0-9]+).*/\2/')
if [ -n "$hmaj" ] && { [ "$hmaj" -gt 3 ] || { [ "$hmaj" -eq 3 ] && [ "$hmin" -ge 14 ]; }; }; then
  check helm-version pass "helm $hv"
else
  check helm-version fail "helm ${hv:-unknown} is older than 3.14" "install Helm 3.14 or newer"
fi

# --- Context and reachability -------------------------------------------------
if [ -z "$KCTX" ]; then KCTX=$(kubectl config current-context 2>/dev/null || true); fi
if [ -z "$KCTX" ]; then
  check kube-context fail "no current kube context" "pass --context, or ask the human which cluster to use"
  emit; exit 3
fi
if ! kubectl config get-contexts "$KCTX" >/dev/null 2>&1; then
  check kube-context fail "context $KCTX not in kubeconfig" "pass an existing --context (kubectl config get-contexts -o name)"
  emit; exit 3
fi
check kube-context pass "$KCTX (confirm with the human that this is the intended cluster)"

ver=$(k get --raw /version 2>/dev/null || true)
if [ -z "$ver" ]; then
  check cluster-reachable fail "cannot reach the API server of $KCTX" "start the cluster or fix credentials (cloud credentials are a human step)"
  emit; exit 3
fi
minor=$(printf '%s' "$ver" | tr -d '\n ' | sed -E 's/.*"minor":"([0-9]+).*/\1/')
gitv=$(printf '%s' "$ver" | tr -d '\n ' | sed -E 's/.*"gitVersion":"([^"]+)".*/\1/')
if [ "${minor:-0}" -ge 27 ] 2>/dev/null; then
  check kube-version pass "server $gitv"
else
  check kube-version fail "server $gitv; the charts need >= 1.27" "upgrade the cluster"
fi

# --- RBAC -------------------------------------------------------------------
missing=""
for res in deployments statefulsets secrets services networkpolicies persistentvolumeclaims configmaps; do
  [ "$(k auth can-i create "$res" -n "$NS" 2>/dev/null || true)" = yes ] || missing="$missing $res"
done
if [ -z "$missing" ]; then
  check rbac pass "can create the chart's resources in $NS"
else
  check rbac fail "cannot create:$missing in $NS" "ask the human for a role that can, or another namespace"
fi

# --- Namespace and Pod Security Admission ------------------------------------
if k get namespace "$NS" >/dev/null 2>&1; then
  enforce=$(k get namespace "$NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null || true)
  if [ "$enforce" = restricted ]; then
    check pod-security pass "namespace $NS enforces restricted"
  else
    check pod-security warn "namespace $NS enforces '${enforce:-nothing}'; the charts are built for restricted" \
      "kubectl --context $KCTX label namespace $NS pod-security.kubernetes.io/enforce=restricted --overwrite (never relax it to make a pod start)"
  fi
else
  if [ "$(k auth can-i create namespaces 2>/dev/null || true)" = yes ]; then
    check pod-security pass "namespace $NS absent; bootstrap-secrets.sh creates it with pod-security enforce=restricted"
  else
    check pod-security fail "namespace $NS absent and you cannot create namespaces" "ask the human to create $NS labelled pod-security.kubernetes.io/enforce=restricted"
  fi
fi

# --- DNS selector the egress policy uses ------------------------------------
if [ -n "$(k -n kube-system get pods -l k8s-app=kube-dns -o name 2>/dev/null || true)" ]; then
  check dns-selector pass "kube-system pods labelled k8s-app=kube-dns exist (the egress policy's DNS rule matches them)"
else
  check dns-selector warn "no kube-system pod labelled k8s-app=kube-dns; the default DNS egress rule would block lookups" \
    "set networkPolicy.dns.namespaceSelector/podSelector to your resolver's labels in an extra values file"
fi

# --- NetworkPolicy enforcement ------------------------------------------------
osimg=$(k get nodes -o jsonpath='{.items[0].status.nodeInfo.osImage}' 2>/dev/null || true)
cni=$(k get pods -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
  | grep -Eo '^(calico|cilium|antrea|kube-router|weave|canal|kindnet|kube-ovn|ovnkube|aws-node|azure-npm|gke-metadata|netpol|flannel)' | sort -u | tr '\n' ' ' || true)
case " $cni " in
  *" calico "*|*" cilium "*|*" antrea "*|*" kube-router "*|*" weave "*|*" canal "*|*" kube-ovn "*|*" ovnkube "*|*" azure-npm "*|*" netpol "*)
    netpol=likely; why="policy-enforcing CNI found: $cni" ;;
  *)
    if [ "$osimg" = OrbStack ]; then netpol=likely; why="OrbStack's built-in network enforces NetworkPolicy"
    elif [ -n "$cni" ]; then netpol=unknown; why="CNI pods found ($cni) may not enforce NetworkPolicy (flannel and older kindnet do not)"
    else netpol=unknown; why="no known CNI pods found"
    fi ;;
esac

# --- Storage ------------------------------------------------------------------
dsc=$(k get storageclass -o jsonpath='{range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{" "}{.provisioner}{"\n"}{end}' 2>/dev/null | head -1 || true)
needs_pvc=1
[ "$SCENARIO" = team-external ] && needs_pvc=0
[ "$SCENARIO" = auth-external ] && needs_pvc=0
if [ -z "$dsc" ]; then
  if [ "$needs_pvc" = 1 ]; then
    check storage-class fail "no default StorageClass" "ask the human which StorageClass to use; pass it in an extra values file (persistence.storageClass / database.*.storage)"
  else
    check storage-class skip "no default StorageClass; $SCENARIO needs none"
  fi
else
  check storage-class pass "default StorageClass ${dsc% *} (provisioner ${dsc#* })"
fi
rwop=unknown
if [ -n "$dsc" ]; then
  prov=${dsc#* }
  if [ "$prov" = rancher.io/local-path ] || k get csidriver "$prov" >/dev/null 2>&1; then rwop=likely; fi
fi

# --- Active probe (opt-in) ----------------------------------------------------
PNS=""
probe_cleanup() { if [ -n "$PNS" ]; then k delete namespace "$PNS" --wait=false >/dev/null 2>&1 || true; PNS=""; fi; }
probe() {
  local pns
  pns=$(printf 'ligus-preflight-%04x%04x' "$RANDOM" "$RANDOM")
  PNS=$pns
  trap probe_cleanup EXIT
  k create namespace "$pns" >/dev/null
  k label namespace "$pns" pod-security.kubernetes.io/enforce=restricted >/dev/null
  local img=docker.io/library/busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662
  local sc='{"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}}'
  local csc='{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}'
  k -n "$pns" apply -f - >/dev/null <<EOF
{"apiVersion":"v1","kind":"List","items":[
 {"apiVersion":"v1","kind":"Pod","metadata":{"name":"srv","labels":{"app":"srv"}},
  "spec":{"securityContext":$sc,"containers":[{"name":"c","image":"$img","securityContext":$csc,
   "command":["sh","-c","mkdir -p /tmp/w && echo ok > /tmp/w/index.html && exec httpd -f -p 8080 -h /tmp/w"]}]}},
 {"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"rwop"},
  "spec":{"accessModes":["ReadWriteOncePod"],"resources":{"requests":{"storage":"16Mi"}}}},
 {"apiVersion":"v1","kind":"Pod","metadata":{"name":"cli"},
  "spec":{"securityContext":$sc,"containers":[{"name":"c","image":"$img","securityContext":$csc,"command":["sleep","600"],
   "volumeMounts":[{"name":"v","mountPath":"/v"}]}],"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"rwop"}}]}}
]}
EOF
  if ! k -n "$pns" wait --for=condition=Ready pod/srv pod/cli --timeout=120s >/dev/null 2>&1; then
    netpol=probe-error; rwop=probe-failed; return
  fi
  rwop=proven
  local ip; ip=$(k -n "$pns" get pod srv -o jsonpath='{.status.podIP}')
  if ! k -n "$pns" exec cli -- wget -qO- -T 3 "http://$ip:8080/" >/dev/null 2>&1; then
    netpol=probe-error; return
  fi
  k -n "$pns" apply -f - >/dev/null <<EOF
{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"deny"},"spec":{"podSelector":{},"policyTypes":["Ingress"]}}
EOF
  local _
  for _ in 1 2 3 4 5 6; do
    sleep 2
    if ! k -n "$pns" exec cli -- wget -qO- -T 2 "http://$ip:8080/" >/dev/null 2>&1; then netpol=proven; return; fi
  done
  netpol=not-enforced
}
if [ "$PROBE" = 1 ]; then probe; probe_cleanup; fi

case $netpol in
  proven) check networkpolicy pass "probe: a deny-all policy blocked traffic" ;;
  likely) check networkpolicy pass "$why (run with --probe to prove it)" ;;
  not-enforced) check networkpolicy fail "probe: traffic still flowed under a deny-all policy; the charts' isolation would be silently absent" \
    "use a cluster whose CNI enforces NetworkPolicy (Calico, Cilium, ...); never set networkPolicy.enabled=false to proceed; ask the human" ;;
  probe-error) check networkpolicy fail "probe pods did not start or could not talk before the policy" "inspect events in a ligus-preflight-* namespace run, or rerun without --probe" ;;
  *) check networkpolicy warn "$why" "rerun with --probe to prove enforcement before trusting the charts' isolation" ;;
esac

uses_rwop=0
case $SCENARIO in team-bundled|auth-bundled) uses_rwop=1 ;; esac
case $rwop in
  proven) check storage-rwop pass "probe: a ReadWriteOncePod volume bound and mounted" ;;
  likely) check storage-rwop pass "provisioner ${dsc#* } supports ReadWriteOncePod (run with --probe to prove it)" ;;
  probe-failed) check storage-rwop "$([ $uses_rwop = 1 ] && echo fail || echo warn)" "probe: the RWOP volume did not mount" \
    "set database.bundled.persistence.accessModes=[ReadWriteOnce] in an extra values file" ;;
  *) check storage-rwop "$([ $uses_rwop = 1 ] && echo warn || echo skip)" "ReadWriteOncePod support unknown for ${dsc#* }" \
    "rerun with --probe, or set database.bundled.persistence.accessModes=[ReadWriteOnce]" ;;
esac

# --- CloudNativePG ------------------------------------------------------------
dbmode=""
[ -n "$SCENARIO" ] && dbmode=$(scenario_dbmode "$SCENARIO")
if [ "$dbmode" = cnpg ]; then
  if k get crd clusters.postgresql.cnpg.io >/dev/null 2>&1; then
    opns=$(k get deploy -A -l app.kubernetes.io/name=cloudnative-pg -o jsonpath='{.items[0].metadata.namespace}' 2>/dev/null || true)
    ready=$(k get deploy -A -l app.kubernetes.io/name=cloudnative-pg -o jsonpath='{.items[0].status.readyReplicas}' 2>/dev/null || true)
    if [ -z "$opns" ]; then
      check cnpg-operator warn "CNPG CRDs present but no operator Deployment labelled app.kubernetes.io/name=cloudnative-pg" "confirm the operator runs before installing"
    elif [ "${ready:-0}" -ge 1 ]; then
      if [ "$opns" = cnpg-system ]; then
        check cnpg-operator pass "operator ready in $opns"
      else
        check cnpg-operator warn "operator ready in $opns, but the chart's instance NetworkPolicy admits cnpg-system" \
          "set database.cnpg.networkPolicy.operatorNamespaceSelector.matchLabels.kubernetes\\.io/metadata\\.name=$opns"
      fi
    else
      check cnpg-operator fail "operator in $opns is not ready" "kubectl --context $KCTX -n $opns get pods"
    fi
  else
    check cnpg-operator fail "CloudNativePG CRDs absent (postgresql.cnpg.io)" \
      "ask the human whether to install the CNPG operator (cluster-wide), or choose team-bundled / team-external"
  fi
else
  check cnpg-operator skip "not needed for ${SCENARIO:-no scenario}"
fi

# --- Exposure (information for the human decision) ----------------------------
ics=$(k get ingressclass -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null || true)
check ingress-classes "$([ -n "$ics" ] && echo pass || echo warn)" "${ics:-none}; exposure (ingress/httpRoute host, TLS, trusted proxies) is a human decision" \
  "$([ -z "$ics" ] && echo 'in-cluster and port-forward access only until the human sets up ingress')"

if command -v openssl >/dev/null 2>&1; then check tool-openssl pass "openssl found"; else check tool-openssl fail "openssl missing (bootstrap-secrets.sh needs it)" "install openssl"; fi

emit
exit "$FAILED"
