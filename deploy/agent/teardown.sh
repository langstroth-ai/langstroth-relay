#!/usr/bin/env bash
# Uninstalls a release. By default keeps data (PVCs) and the bootstrap
# Secrets so a reinstall finds them; each removal beyond the release is an
# explicit flag (the flag is the consent; the agent asks the human first for
# anything holding data that matters). --delete-namespace only deletes a
# namespace bootstrap-secrets.sh created (label ligus.ai/created-by).
#
#   deploy/agent/teardown.sh --context orbstack --namespace langstroth-relay \
#     --release langstroth-relay [--delete-data] [--delete-secrets] [--delete-namespace] [--json]
#
# Exit: 0 done (also when there was nothing to remove), 1 a step failed,
# 2 usage, 3 tool/cluster.
source "$(dirname "$0")/lib.sh"

usage() { echo "usage: $0 --context CTX --namespace NS [--release R] [--delete-data] [--delete-secrets] [--delete-namespace] [--json]" >&2; }
pick_env NS LANGSTROTH_NAMESPACE LIGUS_NAMESPACE
pick_env RELEASE LANGSTROTH_RELEASE LIGUS_RELEASE
DEL_DATA=0 DEL_SEC=0 DEL_NS=0
while [ $# -gt 0 ]; do
  case $1 in
    --context) KCTX=${2:?}; shift 2 ;;
    --namespace) NS=${2:?}; shift 2 ;;
    --release) RELEASE=${2:?}; shift 2 ;;
    --delete-data) DEL_DATA=1; shift ;;
    --delete-secrets) DEL_SEC=1; shift ;;
    --delete-namespace) DEL_NS=1; shift ;;
    --json) JSON=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die_usage "unknown argument $1" ;;
  esac
done
[ -n "$NS" ] || { usage; die_usage "--namespace is required"; }
case $NS in default|kube-system|kube-public|kube-node-lease) [ "$DEL_NS" = 1 ] && die_usage "refusing to delete namespace $NS" ;; esac
RELEASE=${RELEASE:-langstroth-relay}
require_context
valid --namespace "$NS" "$DNS1123"
valid --release "$RELEASE" "$DNS1123"
need_tool kubectl "install kubectl"
need_tool helm "install Helm"
k get --raw /version >/dev/null 2>&1 || { log "cluster for context $KCTX unreachable"; exit 3; }

if ! k get namespace "$NS" >/dev/null 2>&1; then
  check namespace pass "namespace $NS does not exist; nothing to do"
else
  chart=$(h -n "$NS" get metadata "$RELEASE" -o json 2>/dev/null | tr -d '\n' | sed -nE 's/.*"chart":"([^"]+)".*/\1/p' || true)
  if [ -n "$chart" ]; then
    if h -n "$NS" uninstall "$RELEASE" --wait --timeout 5m >/dev/null 2>&1; then
      check uninstall pass "uninstalled $RELEASE ($chart)"
    else
      check uninstall fail "helm uninstall $RELEASE failed" "helm --kube-context $KCTX -n $NS status $RELEASE; kubectl --context $KCTX -n $NS get all"
    fi
  else
    check uninstall pass "no release $RELEASE in $NS"
  fi
  FULL=$(fullname "${chart:-langstroth-relay}" "$RELEASE")
  others=$(h -n "$NS" list -q -a 2>/dev/null | grep -vx "$RELEASE" | tr '\n' ' ' || true)
  if [ "$DEL_NS" = 1 ] && [ -n "$others" ]; then
    check namespace fail "namespace $NS still holds other Helm releases ($others); refusing to delete it with theirs" \
      "tear those down first, or rerun without --delete-namespace; the human decides"
    HUMAN=1
  elif [ "$DEL_NS" = 1 ] && [ "$(k get namespace "$NS" -o jsonpath='{.metadata.labels.ligus\.ai/created-by}' 2>/dev/null)" != bootstrap-secrets ]; then
    check namespace fail "namespace $NS was not created by bootstrap-secrets.sh; refusing to delete it" \
      "rerun without --delete-namespace (add --delete-data/--delete-secrets if the human agreed); the human deletes the namespace"
  elif [ "$DEL_NS" = 1 ]; then
    if k delete namespace "$NS" --wait=true --timeout=5m >/dev/null 2>&1; then
      check namespace pass "deleted namespace $NS (data and Secrets with it)"
    else
      check namespace fail "namespace $NS did not finish deleting" "kubectl --context $KCTX get namespace $NS -o yaml (look for stuck finalizers; do not remove finalizers without the human)"
    fi
  else
    pvcs=$(k -n "$NS" get pvc -o name 2>/dev/null | release_pvcs "$FULL")
    if [ "$DEL_DATA" = 1 ] && [ -n "$pvcs" ]; then
      # shellcheck disable=SC2086
      k -n "$NS" delete $pvcs --wait=true >/dev/null && check data pass "deleted $(echo $pvcs | wc -w | tr -d ' ') PVC(s)"
    elif [ -n "$pvcs" ]; then
      check data pass "kept PVC(s): $(echo "$pvcs" | tr '\n' ' ')(pass --delete-data to remove; the human decides)"
    else
      check data pass "no PVCs left"
    fi
    if [ "$DEL_SEC" = 1 ]; then
      k -n "$NS" delete secret -l "ligus.ai/bootstrap-for=$FULL" >/dev/null 2>&1 || true
      check secrets pass "deleted bootstrap Secrets for $FULL"
    else
      n=$(k -n "$NS" get secret -l "ligus.ai/bootstrap-for=$FULL" -o name 2>/dev/null | wc -l | tr -d ' ')
      check secrets pass "kept $n bootstrap Secret(s) (pass --delete-secrets to remove)"
    fi
  fi
fi

if [ "$JSON" = 1 ]; then
  ok=true; [ "$FAILED" = 1 ] && ok=false
  printf '{"tool":"teardown","ok":%s,"context":%s,"namespace":%s,"release":%s,"checks":%s}\n' \
    "$ok" "$(json_escape "$KCTX")" "$(json_escape "$NS")" "$(json_escape "$RELEASE")" "$(checks_json)"
fi
[ "${HUMAN:-0}" = 1 ] && exit 4
exit "$FAILED"
