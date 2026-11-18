#!/usr/bin/env bash
# Verifies an installed release: Helm status, pod readiness, helm test,
# health through the Service (port-forward), /metrics gated by its token,
# no secret in any pod environment, NetworkPolicies present, namespace on
# Pod Security "restricted". Read-only apart from helm test's own test pod
# and a short-lived port-forward.
#
#   deploy/agent/verify.sh --context orbstack --namespace langstroth-relay \
#     --release langstroth-relay [--json]
#
# Exit: 0 healthy, 1 a check failed (see "fix"), 2 usage, 3 tool/cluster.
source "$(dirname "$0")/lib.sh"

usage() { echo "usage: $0 --context CTX --namespace NS [--release R] [--skip-helm-test] [--json]" >&2; }

pick_env NS LANGSTROTH_NAMESPACE LIGUS_NAMESPACE
pick_env RELEASE LANGSTROTH_RELEASE LIGUS_RELEASE
SKIP_TEST=0
while [ $# -gt 0 ]; do
  case $1 in
    --context) KCTX=${2:?}; shift 2 ;;
    --namespace) NS=${2:?}; shift 2 ;;
    --release) RELEASE=${2:?}; shift 2 ;;
    --skip-helm-test) SKIP_TEST=1; shift ;;
    --json) JSON=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die_usage "unknown argument $1" ;;
  esac
done
[ -n "$NS" ] || { usage; die_usage "--namespace is required"; }
RELEASE=${RELEASE:-langstroth-relay}
require_context
valid --namespace "$NS" "$DNS1123"
valid --release "$RELEASE" "$DNS1123"
need_tool kubectl "install kubectl"
need_tool helm "install Helm"
need_tool curl "install curl"

TMP=$(mktemp -d); chmod 700 "$TMP"
PF=""
# shellcheck disable=SC2329 # run by the EXIT trap
cleanup() { if [ -n "$PF" ]; then kill "$PF" >/dev/null 2>&1 || true; fi; rm -rf "$TMP"; }
trap cleanup EXIT

emit() {
  if [ "$JSON" = 1 ]; then
    local ok=true; [ "$FAILED" = 1 ] && ok=false
    printf '{"tool":"verify","ok":%s,"context":%s,"namespace":%s,"release":%s,"chart":%s,"checks":%s}\n' \
      "$ok" "$(json_escape "$KCTX")" "$(json_escape "$NS")" "$(json_escape "$RELEASE")" "$(json_escape "${CHARTNAME-}")" "$(checks_json)"
  fi
  exit "$FAILED"
}

# --- Helm release ------------------------------------------------------------------
st=$(h -n "$NS" status "$RELEASE" -o json 2>/dev/null || true)
if [ -z "$st" ]; then
  check helm-release fail "no release $RELEASE in $NS (context $KCTX)" "deploy/agent/install.sh ... or check --namespace/--release"
  emit
fi
status=$(printf '%s' "$st" | tr -d '\n' | sed -nE 's/.*"info":\{[^}]*"status":"([a-z-]+)".*/\1/p')
CHARTNAME=$(h -n "$NS" get metadata "$RELEASE" -o json 2>/dev/null | tr -d '\n' | sed -nE 's/.*"chart":"([^"]+)".*/\1/p')
if [ "$status" = deployed ]; then check helm-release pass "$RELEASE is deployed ($CHARTNAME)"
else check helm-release fail "$RELEASE status is ${status:-unknown}" "helm --kube-context $KCTX -n $NS history $RELEASE; roll back or reinstall"
fi
FULL=$(fullname "$CHARTNAME" "$RELEASE")

# --- Pods ------------------------------------------------------------------------
notready=$(k -n "$NS" get pods -l "app.kubernetes.io/instance=$RELEASE" \
  -o jsonpath='{range .items[?(@.status.phase!="Succeeded")]}{.metadata.name}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null \
  | grep -v ' True$' | grep -v '^$' | grep -v -- '-test-' || true)
total=$(k -n "$NS" get pods -l "app.kubernetes.io/instance=$RELEASE" --field-selector=status.phase=Running -o name 2>/dev/null | wc -l | tr -d ' ')
if [ -z "$notready" ] && [ "$total" -gt 0 ]; then check pods-ready pass "$total running pod(s) ready"
else check pods-ready fail "not ready: ${notready:-no running pods}" "kubectl --context $KCTX -n $NS describe pods; kubectl --context $KCTX -n $NS get events --sort-by=.lastTimestamp"
fi

# --- CloudNativePG database (database.mode=cnpg) ---------------------------------
if k api-resources --api-group=postgresql.cnpg.io -o name 2>/dev/null | grep -q '^clusters'; then
  cl=$(k -n "$NS" get clusters.postgresql.cnpg.io "$FULL-db" -o jsonpath='{.status.phase}|{.status.readyInstances}/{.status.instances}' 2>/dev/null || true)
  if [ -n "$cl" ]; then
    if [ "${cl%%|*}" = "Cluster in healthy state" ]; then check database pass "CNPG cluster $FULL-db healthy (${cl#*|} ready)"
    else check database fail "CNPG cluster $FULL-db: ${cl%%|*} (${cl#*|} ready)" "kubectl --context $KCTX -n $NS describe clusters.postgresql.cnpg.io $FULL-db"
    fi
  fi
fi

# --- helm test ------------------------------------------------------------------
if [ "$SKIP_TEST" = 1 ]; then check helm-test skip "--skip-helm-test"
elif [ -n "$(h -n "$NS" get hooks "$RELEASE" 2>/dev/null | grep 'helm.sh/hook: test' || true)" ]; then
  # Retried: on some CNIs a NetworkPolicy reaches a just-started pod a few
  # seconds after it reports Ready, so the first test right after an install
  # can be refused.
  ok=0
  for attempt in 1 2 3; do
    if out=$(h -n "$NS" test "$RELEASE" --logs --timeout 90s 2>&1); then ok=1; break; fi
    [ "$attempt" -lt 3 ] && sleep 10
  done
  if [ "$ok" = 1 ]; then check helm-test pass "chart tests passed (attempt $attempt)"
  else log "$out"; check helm-test fail "helm test failed 3 times" "helm --kube-context $KCTX -n $NS test $RELEASE --logs"
  fi
else check helm-test skip "chart $CHARTNAME has no test hooks"
fi

# --- Health through the Service ---------------------------------------------------
port_forward() { # svc remoteport -> sets LPORT
  LPORT=$((20000 + RANDOM % 20000))
  k -n "$NS" port-forward "svc/$1" "$LPORT:$2" >/dev/null 2>&1 &
  PF=$!
  local _
  for _ in $(seq 40); do curl -s -o /dev/null "http://127.0.0.1:$LPORT/" && return 0; sleep 0.25; done
  return 1
}
stop_pf() { if [ -n "$PF" ]; then kill "$PF" >/dev/null 2>&1 || true; wait "$PF" 2>/dev/null || true; fi; PF=""; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }

if [ "$CHARTNAME" = langstroth-relay ]; then
  svc=$FULL
  k -n "$NS" get svc "$FULL-sync" >/dev/null 2>&1 && svc="$FULL-sync"
  if port_forward "$svc" 8443; then
    c=$(code "http://127.0.0.1:$LPORT/v1/health")
    if [ "$c" = 200 ]; then check health pass "svc/$svc /v1/health 200"
    else check health fail "svc/$svc /v1/health $c" "kubectl --context $KCTX -n $NS logs svc/$svc"
    fi
    c=$(code "http://127.0.0.1:$LPORT/metrics")
    if [ "$c" = 200 ]; then
      check metrics-gated fail "/metrics answers without a token" "set LANGSTROTH_RELAY_METRICS_TOKEN in the $FULL-env Secret (bootstrap-secrets.sh does)"
    else
      # The token goes to curl through a header file, never argv.
      # LIGUS_RELAY_METRICS_TOKEN: a Secret made before the Langstroth rename.
      if { k -n "$NS" get secret "$FULL-env" -o jsonpath='{.data.LANGSTROTH_RELAY_METRICS_TOKEN}' 2>/dev/null | base64 -d >"$TMP/tok" 2>/dev/null && [ -s "$TMP/tok" ]; } ||
        { k -n "$NS" get secret "$FULL-env" -o jsonpath='{.data.LIGUS_RELAY_METRICS_TOKEN}' 2>/dev/null | base64 -d >"$TMP/tok" 2>/dev/null && [ -s "$TMP/tok" ]; }; then
        { printf 'Authorization: Bearer '; cat "$TMP/tok"; printf '\n'; } >"$TMP/hdr"; rm -f "$TMP/tok"
        c2=$(code -H @"$TMP/hdr" "http://127.0.0.1:$LPORT/metrics")
        if [ "$c2" = 200 ]; then check metrics-gated pass "/metrics $c without the token, 200 with it"
        else check metrics-gated fail "/metrics $c2 with the token" "check the $FULL-env Secret is the one in config.existingSecret"
        fi
      else
        check metrics-gated pass "/metrics $c without a token (token Secret not readable here)"
      fi
    fi
    stop_pf
  else
    check health fail "port-forward to svc/$svc failed" "kubectl --context $KCTX -n $NS get svc,endpoints"
  fi
  if k -n "$NS" get svc "$FULL-tunnel" >/dev/null 2>&1 && port_forward "$FULL-tunnel" 8443; then
    c=$(code "http://127.0.0.1:$LPORT/v1/health")
    if [ "$c" = 200 ]; then check tunnel-health pass "svc/$FULL-tunnel /v1/health 200"; else check tunnel-health fail "svc/$FULL-tunnel /v1/health $c" "kubectl --context $KCTX -n $NS logs svc/$FULL-tunnel"; fi
    stop_pf
  fi
elif [ "$CHARTNAME" = langstroth-auth ]; then
  if port_forward "$FULL" 8090; then
    c=$(code "http://127.0.0.1:$LPORT/healthz")
    if [ "$c" = 200 ]; then check health pass "svc/$FULL /healthz 200"; else check health fail "svc/$FULL /healthz $c" "kubectl --context $KCTX -n $NS logs deploy/$FULL"; fi
    stop_pf
  else
    check health fail "port-forward to svc/$FULL failed" "kubectl --context $KCTX -n $NS get svc,endpoints"
  fi
fi

# --- No secrets in the environment ----------------------------------------------
if k -n "$NS" get pods -l "app.kubernetes.io/instance=$RELEASE" -o yaml 2>/dev/null | grep -q secretKeyRef; then
  check no-secret-env fail "a pod takes a Secret through an environment variable" "secrets must be mounted files; do not add env secretKeyRef overrides"
else
  check no-secret-env pass "no secretKeyRef in pod environments"
fi

# --- NetworkPolicy and Pod Security ----------------------------------------------
np=$(k -n "$NS" get networkpolicy -l "app.kubernetes.io/instance=$RELEASE" -o name 2>/dev/null | wc -l | tr -d ' ')
if [ "$np" -gt 0 ]; then check networkpolicy pass "$np NetworkPolicies for $RELEASE"
else check networkpolicy fail "no NetworkPolicy for $RELEASE" "networkPolicy.enabled must stay true; reinstall without overriding it"
fi
enf=$(k get namespace "$NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>/dev/null || true)
if [ "$enf" = restricted ]; then check pod-security pass "$NS enforces restricted"
else check pod-security warn "$NS enforces '${enf:-nothing}'" "kubectl --context $KCTX label namespace $NS pod-security.kubernetes.io/enforce=restricted --overwrite"
fi

emit
