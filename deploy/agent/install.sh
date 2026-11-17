#!/usr/bin/env bash
# Installs (or upgrades) one scenario: preflight, required-Secret check,
# helm template (client render), helm upgrade --install --dry-run=server,
# then the real helm upgrade --install --wait. --dry-run stops before the
# real install.
#
#   deploy/agent/install.sh --context orbstack --namespace ligus-relay \
#     --scenario team-bundled [--values my.yaml] [--dry-run] [--json]
#
# Images: the charts default to ghcr.io/ligus-ai/<chart>:<appVersion>. Use
# --build-image to build them from this checkout (docker); on a kind context
# they are loaded into the cluster, on OrbStack / Docker Desktop the cluster
# shares the docker image store. Or --image-repo/--image-tag.
#
# Exit: 0 installed (or dry run passed), 1 a step failed (see "fix"),
# 2 usage, 3 tool/cluster.
source "$(dirname "$0")/lib.sh"

usage() {
  cat >&2 <<EOF
usage: $0 --scenario S [--context CTX] [--namespace NS] [--release R]
          [--values FILE]... [--image-repo REPO] [--image-tag TAG] [--build-image]
          [--timeout 10m] [--dry-run] [--skip-preflight] [--json]
  scenarios: $SCENARIOS
EOF
}

NS=${LIGUS_NAMESPACE:-}
SCENARIO=${LIGUS_SCENARIO:-}
RELEASE=${LIGUS_RELEASE:-}
EXTRA=()
IMG_REPO=${LIGUS_IMAGE_REPO:-}
IMG_TAG=${LIGUS_IMAGE_TAG:-}
BUILD=0
TIMEOUT=10m
DRY=0
SKIP_PRE=0
while [ $# -gt 0 ]; do
  case $1 in
    --context) KCTX=${2:?}; shift 2 ;;
    --namespace) NS=${2:?}; shift 2 ;;
    --scenario) SCENARIO=${2:?}; shift 2 ;;
    --release) RELEASE=${2:?}; shift 2 ;;
    --values|-f) EXTRA+=("${2:?}"); shift 2 ;;
    --image-repo) IMG_REPO=${2:?}; shift 2 ;;
    --image-tag) IMG_TAG=${2:?}; shift 2 ;;
    --build-image) BUILD=1; shift ;;
    --timeout) TIMEOUT=${2:?}; shift 2 ;;
    --dry-run) DRY=1; shift ;;
    --skip-preflight) SKIP_PRE=1; shift ;;
    --json) JSON=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die_usage "unknown argument $1" ;;
  esac
done
[ -n "$SCENARIO" ] || { usage; die_usage "--scenario is required"; }
valid_scenario "$SCENARIO" || die_usage "unknown scenario $SCENARIO (one of: $SCENARIOS)"
CHART=$(scenario_chart "$SCENARIO")
CHARTNAME=$(basename "$CHART")
NS=${NS:-$CHARTNAME}
RELEASE=${RELEASE:-$CHARTNAME}
FULL=$(fullname "$CHARTNAME" "$RELEASE")
DBMODE=$(scenario_dbmode "$SCENARIO")
for f in "${EXTRA[@]+"${EXTRA[@]}"}"; do [ -r "$f" ] || die_usage "values file $f not readable"; done

need_tool kubectl "install kubectl"
need_tool helm "install Helm 3.14+"
[ -n "$KCTX" ] || KCTX=$(kubectl config current-context 2>/dev/null || true)
[ -n "$KCTX" ] || { log "no kube context; pass --context"; exit 3; }

STEP=""
REVISION=""
OBJECTS=0
finish() { # ok detail fix
  if [ "$JSON" = 1 ]; then
    printf '{"tool":"install","ok":%s,"context":%s,"namespace":%s,"release":%s,"scenario":%s,"chart":%s,"dry_run":%s,"step":%s,"revision":%s,"rendered_objects":%s,"detail":%s,"fix":%s,"next":%s}\n' \
      "$1" "$(json_escape "$KCTX")" "$(json_escape "$NS")" "$(json_escape "$RELEASE")" "$(json_escape "$SCENARIO")" \
      "$(json_escape "$CHARTNAME")" "$([ "$DRY" = 1 ] && echo true || echo false)" "$(json_escape "$STEP")" \
      "$(json_escape "$REVISION")" "$OBJECTS" "$(json_escape "$2")" "$(json_escape "${3-}")" \
      "$(json_escape "deploy/agent/verify.sh --context $KCTX --namespace $NS --release $RELEASE --json")"
  else
    log "$([ "$1" = true ] && echo OK || echo FAILED) [$STEP] $2${3:+ -> $3}"
  fi
  [ "$1" = true ] && exit 0 || exit 1
}

# 1. Preflight -------------------------------------------------------------------
STEP=preflight
if [ "$SKIP_PRE" = 0 ]; then
  pre=$("$AGENT_DIR/preflight.sh" --context "$KCTX" --namespace "$NS" --scenario "$SCENARIO" --json) || {
    rc=$?
    [ "$rc" -ge 2 ] && { printf '%s\n' "$pre" >&2; exit "$rc"; }
    log "$pre"
    finish false "preflight failed" "run deploy/agent/preflight.sh --context $KCTX --namespace $NS --scenario $SCENARIO and resolve each failed check (ask the human where it says so)"
  }
fi

# 2. Human inputs and Secrets ---------------------------------------------------
STEP=inputs
if [[ $SCENARIO == auth-* ]] && [ "${#EXTRA[@]}" -eq 0 ]; then
  finish false "auth scenarios need the human's values file (public_url, IdP tenants, trusted proxies, ingress)" \
    "ask the human to fill in a copy of deploy/agent/values/auth-example-human.yaml, then pass it with --values"
fi
if [ "$SCENARIO" = team-cnpg ] && [ "${#EXTRA[@]}" -eq 0 ]; then
  finish false "team-cnpg needs the human's backup destination (database.cnpg.backups.barmanObjectStore)" \
    "ask the human for the object store and its credentials Secret; pass them in a values file with --values (or use team-cnpg-eval to evaluate)"
fi
if [ "$SCENARIO" = team-external ] && [ "${#EXTRA[@]}" -eq 0 ]; then
  finish false "team-external needs database.external.cidrs from the human" "pass a values file with database.external.cidrs=[<db address>/32]"
fi
missing=""
for s in $(required_secrets "$SCENARIO" "$FULL"); do
  k -n "$NS" get secret "$s" >/dev/null 2>&1 || missing="$missing $s"
done
if [ -n "$missing" ]; then
  finish false "missing Secrets:$missing" "deploy/agent/bootstrap-secrets.sh --context $KCTX --namespace $NS --scenario $SCENARIO --release $RELEASE"
fi

# 3. Image ------------------------------------------------------------------------
STEP=image
if [ "$BUILD" = 1 ]; then
  need_tool docker "install docker (or pass --image-repo/--image-tag of a pushed image)"
  IMG_REPO=$CHARTNAME
  IMG_TAG=${IMG_TAG:-dev}
  df=deploy/container/Dockerfile
  [ "$CHARTNAME" = ligus-auth ] && df=deploy/container/Dockerfile.ligus-auth
  (cd "$REPO_ROOT" && docker build -q -f "$df" -t "$IMG_REPO:$IMG_TAG" . >&2) || finish false "docker build failed" "read the build output above"
  case $KCTX in
    kind-*) need_tool kind "install kind"; kind load docker-image "$IMG_REPO:$IMG_TAG" --name "${KCTX#kind-}" >&2 ;;
    k3d-*) need_tool k3d "install k3d"; k3d image import "$IMG_REPO:$IMG_TAG" -c "${KCTX#k3d-}" >&2 ;;
  esac
fi

# 4. Helm arguments -------------------------------------------------------------
ARGS=()
[[ $SCENARIO == auth-* ]] && ARGS+=(-f "$AGENT_DIR/values/auth-common.yaml")
ARGS+=(-f "$(scenario_values "$SCENARIO")")
for f in "${EXTRA[@]+"${EXTRA[@]}"}"; do ARGS+=(-f "$f"); done
# Secret NAMES only (never values) go on the command line.
if [ "$CHARTNAME" = ligus-relay ]; then
  ARGS+=(--set "config.existingSecret=$FULL-env")
else
  ARGS+=(--set "signingKey.existingSecret=$FULL-signing-key" --set "adminToken.existingSecret=$FULL-admin")
  k -n "$NS" get secret "$FULL-secrets" >/dev/null 2>&1 && ARGS+=(--set "secretFiles.existingSecret=$FULL-secrets")
fi
case $DBMODE in
  bundled) ARGS+=(--set "database.bundled.auth.existingSecret=$FULL-postgres-auth" --set "database.bundled.tls.existingSecret=$FULL-postgres-tls") ;;
  external) ARGS+=(--set "database.external.existingSecret=$FULL-database") ;;
esac
[ -n "$IMG_REPO" ] && ARGS+=(--set "image.repository=$IMG_REPO")
[ -n "$IMG_TAG" ] && ARGS+=(--set "image.tag=$IMG_TAG")
if [ "$BUILD" = 1 ]; then ARGS+=(--set image.pullPolicy=Never); fi

# 5. Client-side render ---------------------------------------------------------
STEP=template
apiv=()
if [ "$DBMODE" = cnpg ] && k get crd clusters.postgresql.cnpg.io >/dev/null 2>&1; then apiv=(--api-versions postgresql.cnpg.io/v1); fi
# helm template renders for an old default Kubernetes; render for the cluster's.
kv=$(k get --raw /version 2>/dev/null | tr -d '\n ' | sed -nE 's/.*"gitVersion":"v?([0-9]+\.[0-9]+\.[0-9]+).*/\1/p')
[ -n "$kv" ] && apiv+=(--kube-version "$kv")
if ! out=$(h template "$RELEASE" "$CHART" -n "$NS" "${ARGS[@]}" "${apiv[@]+"${apiv[@]}"}" 2>&1); then
  log "$out"
  finish false "helm template refused the values" "read the chart's message above; it names the setting. Do not disable a safety check to get past it; ask the human"
fi
OBJECTS=$(printf '%s\n' "$out" | grep -c '^kind:' || true)

# 6. Server-side dry run --------------------------------------------------------
STEP=server-dry-run
if ! out=$(h upgrade --install "$RELEASE" "$CHART" -n "$NS" "${ARGS[@]}" --dry-run=server 2>&1); then
  log "$out"
  finish false "the API server rejected the rendered objects" "read the error above (admission, PSA, quota, missing CRD)"
fi
if [ "$DRY" = 1 ]; then finish true "dry run passed: $OBJECTS objects render and the API server accepts them"; fi

# 7. Install ------------------------------------------------------------------------
STEP=install
if ! h upgrade --install "$RELEASE" "$CHART" -n "$NS" "${ARGS[@]}" --wait --timeout "$TIMEOUT" >&2; then
  finish false "helm upgrade --install did not become ready within $TIMEOUT" \
    "kubectl --context $KCTX -n $NS get pods,events; on a failed upgrade: helm --kube-context $KCTX -n $NS rollback $RELEASE; on a failed first install: deploy/agent/teardown.sh"
fi
REVISION=$(h -n "$NS" status "$RELEASE" -o json 2>/dev/null | sed -nE 's/.*"version":([0-9]+).*/\1/p' | head -1)
finish true "installed $RELEASE (revision ${REVISION:-?}) in $NS"
