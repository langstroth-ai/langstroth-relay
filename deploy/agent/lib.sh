# shellcheck shell=bash
# shellcheck disable=SC2034 # FAILED and others are read by the sourcing scripts
# Shared helpers for the deploy/agent scripts. Sourced, never run.
#
# Conventions every script follows (AGENTS.md, "Tooling contract"):
#   - non-interactive: flags or environment only, never a prompt;
#   - idempotent: running twice leaves the same state;
#   - --json prints exactly one JSON object on stdout; human logs go to stderr;
#   - exit codes: 0 ok, 1 a check or step failed (see "fix" in the output),
#     2 usage error, 3 a required tool is missing or the cluster is unreachable;
#   - no secret value is ever printed, logged or passed on a command line.

set -o errexit -o nounset -o pipefail

AGENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$AGENT_DIR/../.." && pwd)"
RELAY_CHART="$REPO_ROOT/deploy/helm/ligus-relay"
AUTH_CHART="$REPO_ROOT/deploy/helm/ligus-auth"
JSON=0

log() { printf '%s\n' "$*" >&2; }

# json_escape STRING: a JSON string literal (with quotes).
json_escape() {
  local s=${1-}
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\r'/\\r}
  s=${s//$'\t'/\\t}
  printf '"%s"' "$s"
}

# Check results: check NAME STATUS DETAIL [FIX]. STATUS is pass, warn, fail
# or skip. Rendered by checks_json.
CHECKS=()
FAILED=0 # read by the scripts that source this file
check() {
  local name=$1 status=$2 detail=$3 fix=${4-}
  if [ "$status" = fail ]; then FAILED=1; fi
  CHECKS+=("{\"name\":$(json_escape "$name"),\"status\":$(json_escape "$status"),\"detail\":$(json_escape "$detail"),\"fix\":$(json_escape "$fix")}")
  if [ "$JSON" = 0 ]; then
    if [ -n "$fix" ] && [ "$status" != pass ]; then
      log "[$status] $name: $detail -> $fix"
    else
      log "[$status] $name: $detail"
    fi
  fi
}
checks_json() {
  local IFS=,
  printf '[%s]' "${CHECKS[*]-}"
}

die_usage() { log "usage error: $*"; exit 2; }

need_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    log "missing tool: $1 ($2)"
    if [ "$JSON" = 1 ]; then
      printf '{"ok":false,"error":%s}\n' "$(json_escape "missing tool $1: $2")"
    fi
    exit 3
  }
}

# kubectl/helm bound to one context, so a script never acts on whatever
# context happens to be current when it was started.
KCTX=""
k() { if [ -n "$KCTX" ]; then kubectl --context "$KCTX" "$@"; else kubectl "$@"; fi; }
h() { if [ -n "$KCTX" ]; then helm --kube-context "$KCTX" "$@"; else helm "$@"; fi; }

# Scenario -> chart and values file. Scenarios are documented in AGENTS.md.
scenario_chart() {
  case $1 in
    auth-*) printf '%s' "$AUTH_CHART" ;;
    *) printf '%s' "$RELAY_CHART" ;;
  esac
}
scenario_chartname() { basename "$(scenario_chart "$1")"; }
scenario_values() { printf '%s/values/%s.yaml' "$AGENT_DIR" "$1"; }
scenario_dbmode() {
  case $1 in
    eval-memory) printf none ;;
    team-bundled|auth-bundled) printf bundled ;;
    team-cnpg|team-cnpg-eval|auth-cnpg-eval) printf cnpg ;;
    team-external|auth-external) printf external ;;
    auth-file) printf none ;;
    *) return 1 ;;
  esac
}
SCENARIOS="eval-memory team-bundled team-cnpg-eval team-cnpg team-external auth-file auth-bundled auth-cnpg-eval auth-external"
valid_scenario() { case " $SCENARIOS " in *" $1 "*) return 0 ;; esac; return 1; }

# Secret names the scenario values files reference, by role. The bootstrap
# script creates them and install refuses to start without them.
# fullname CHART RELEASE: the chart's resource-name prefix (its fullname helper).
fullname() {
  case $2 in
    *"$1"*) printf '%s' "${2:0:63}" ;;
    *) local n="$2-$1"; printf '%s' "${n:0:63}" ;;
  esac
}
required_secrets() { # scenario fullname
  local s=$1 r=$2
  case $s in
    eval-memory) echo "$r-env" ;;
    team-bundled) echo "$r-env $r-postgres-auth $r-postgres-tls" ;;
    team-cnpg|team-cnpg-eval) echo "$r-env" ;;
    team-external) echo "$r-env $r-database" ;;
    auth-file) echo "$r-signing-key $r-admin" ;;
    auth-bundled) echo "$r-signing-key $r-admin $r-postgres-auth $r-postgres-tls" ;;
    auth-cnpg-eval) echo "$r-signing-key $r-admin" ;;
    auth-external) echo "$r-signing-key $r-admin $r-database" ;;
  esac
}
