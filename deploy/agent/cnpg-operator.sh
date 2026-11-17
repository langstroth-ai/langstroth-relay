#!/usr/bin/env bash
# The CloudNativePG operator, for database.mode=cnpg. It is CLUSTER-WIDE
# (CRDs, a controller with cluster RBAC): installing or removing it is the
# human's decision, never an agent's default. Removing it while any cluster
# still has CNPG databases strands them; uninstall refuses in that case.
#
#   deploy/agent/cnpg-operator.sh status|install|uninstall [--context CTX] [--json]
#
# install is a no-op when the CRDs already exist (whoever installed them).
# uninstall removes only a release this script installed (Helm release
# "cnpg" in cnpg-system, labelled by this script).
# Exit: 0 ok, 1 failed or refused, 2 usage, 3 tool/cluster.
source "$(dirname "$0")/lib.sh"

CNPG_CHART_VERSION=${CNPG_CHART_VERSION:-0.29.2}   # operator 1.30.2
OPNS=cnpg-system
ACTION=${1:-}
[ $# -gt 0 ] && shift
while [ $# -gt 0 ]; do
  case $1 in
    --context) KCTX=${2:?}; shift 2 ;;
    --json) JSON=1; shift ;;
    *) die_usage "unknown argument $1" ;;
  esac
done
case $ACTION in status|install|uninstall) ;; *) die_usage "usage: $0 status|install|uninstall [--context CTX] [--json]" ;; esac
need_tool kubectl "install kubectl"
need_tool helm "install Helm"
[ -n "$KCTX" ] || KCTX=$(kubectl config current-context 2>/dev/null || true)
k get --raw /version >/dev/null 2>&1 || { log "cluster for context $KCTX unreachable"; exit 3; }

crd=0; k get crd clusters.postgresql.cnpg.io >/dev/null 2>&1 && crd=1
ours=0; h -n "$OPNS" status cnpg >/dev/null 2>&1 && \
  [ "$(k get namespace "$OPNS" -o jsonpath='{.metadata.labels.ligus\.ai/installed-by}' 2>/dev/null)" = cnpg-operator.sh ] && ours=1

case $ACTION in
  status)
    if [ "$crd" = 1 ]; then check cnpg pass "CRDs present$([ "$ours" = 1 ] && echo '; installed by this script')"
    else check cnpg warn "CloudNativePG not installed" "the human decides: deploy/agent/cnpg-operator.sh install --context $KCTX"; fi ;;
  install)
    if [ "$crd" = 1 ]; then check cnpg pass "already installed; left unchanged"
    else
      if h install cnpg cloudnative-pg --repo https://cloudnative-pg.github.io/charts --version "$CNPG_CHART_VERSION" \
           -n "$OPNS" --create-namespace --wait --timeout 5m >/dev/null 2>&1; then
        k label namespace "$OPNS" ligus.ai/installed-by=cnpg-operator.sh --overwrite >/dev/null
        check cnpg pass "installed operator chart $CNPG_CHART_VERSION in $OPNS"
      else
        check cnpg fail "helm install of the CNPG operator failed" "helm --kube-context $KCTX -n $OPNS status cnpg; network access to cloudnative-pg.github.io is needed"
      fi
    fi ;;
  uninstall)
    if [ "$ours" = 0 ]; then check cnpg pass "not installed by this script; left unchanged"
    else
      n=$(k get clusters.postgresql.cnpg.io -A -o name 2>/dev/null | wc -l | tr -d ' ')
      if [ "$n" -gt 0 ]; then
        check cnpg fail "$n CNPG cluster(s) still exist" "tear those releases down first (deploy/agent/teardown.sh)"
      else
        h -n "$OPNS" uninstall cnpg --wait --timeout 5m >/dev/null 2>&1 || true
        # Helm keeps CRDs; remove them so the cluster is as before.
        for c in $(k get crd -o name | grep 'cnpg\.io' || true); do k delete "$c" >/dev/null 2>&1 || true; done
        k delete namespace "$OPNS" --wait=true --timeout=3m >/dev/null 2>&1 || true
        check cnpg pass "removed the operator, its CRDs and $OPNS"
      fi
    fi ;;
esac
if [ "$JSON" = 1 ]; then
  ok=true; [ "$FAILED" = 1 ] && ok=false
  printf '{"tool":"cnpg-operator","ok":%s,"context":%s,"action":%s,"checks":%s}\n' "$ok" "$(json_escape "$KCTX")" "$(json_escape "$ACTION")" "$(checks_json)"
fi
exit "$FAILED"
