# shellcheck shell=bash
# shellcheck disable=SC2034 # FAILED and others are read by the sourcing scripts
# Shared helpers for the deploy/agent scripts. Sourced, never run.
#
# Conventions every script follows (AGENTS.md, "Tooling contract"):
#   - non-interactive: flags or environment only, never a prompt;
#   - idempotent: running twice leaves the same state;
#   - --json prints exactly one JSON object on stdout; human logs go to stderr;
#   - exit codes: 0 ok, 1 a check or step failed (see "fix" in the output),
#     2 usage error, 3 a required tool is missing or the cluster is unreachable,
#     4 a human must decide (a fail-closed default is off, or the step would
#     touch something these scripts did not create): stop and ask;
#   - no secret value is ever printed, logged or passed on a command line.

set -o errexit -o nounset -o pipefail

AGENT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$AGENT_DIR/../.." && pwd)"
RELAY_CHART="$REPO_ROOT/deploy/helm/langstroth-relay"
AUTH_CHART="$REPO_ROOT/deploy/helm/langstroth-auth"
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
# or skip. Rendered by checks_json: each check has "id" (the ligus.checks.v1
# key) and "name" (the same value; the key these scripts always printed).
CHECKS=()
FAILED=0 # read by the scripts that source this file
check() {
  local name=$1 status=$2 detail=$3 fix=${4-}
  if [ "$status" = fail ]; then FAILED=1; fi
  CHECKS+=("{\"id\":$(json_escape "$name"),\"name\":$(json_escape "$name"),\"status\":$(json_escape "$status"),\"detail\":$(json_escape "$detail"),\"fix\":$(json_escape "$fix")}")
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

# pick_env VAR NEW OLD [DEFAULT]: sets VAR to $NEW, else to $OLD (the name
# before the Langstroth rename, e.g. LIGUS_NAMESPACE), else to DEFAULT. Both
# set to different values is a usage error rather than a guess.
pick_env() {
  local new=${!2-} old=${!3-}
  if [ -n "$new" ] && [ -n "$old" ] && [ "$new" != "$old" ]; then
    die_usage "$2 and $3 are both set, to different values: set only $2"
  fi
  printf -v "$1" '%s' "${new:-${old:-${4-}}}"
}

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
# context happens to be current when it was started. Every cluster script
# calls require_context after parsing its flags.
KCTX=""
k() { kubectl --context "$KCTX" "$@"; }
h() { helm --kube-context "$KCTX" "$@"; }
require_context() {
  [ -n "$KCTX" ] || die_usage "--context is required; the scripts never use the current context (kubectl config get-contexts -o name lists them; confirm the cluster with the human)"
  valid --context "$KCTX" '[A-Za-z0-9@:._/-]+'
}

# valid FLAG VALUE REGEX: flag values reach helm --set, labels and messages,
# so they must be plain tokens (a comma in a --set value adds another key).
valid() {
  [ -z "$2" ] && return 0
  printf '%s' "$2" | LC_ALL=C grep -Eq "^($3)\$" && [ "$(printf '%s' "$2" | wc -l | tr -d ' ')" = 0 ] \
    || die_usage "$1: invalid value (allowed: $3)"
}
DNS1123='[a-z0-9]([-a-z0-9]*[a-z0-9])?'
IMAGE_REPO_RE='[A-Za-z0-9][A-Za-z0-9._/:-]*'
IMAGE_TAG_RE='[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}'

# release_pvcs FULL: from "persistentvolumeclaim/NAME" lines on stdin, the
# ones this release's charts create (exact names, never a prefix match: a
# release named langstroth-relay must not take langstroth-relay-b's data with it).
release_pvcs() {
  grep -E "^persistentvolumeclaim/($1|$1-data|data-$1-postgres-[0-9]+|$1-db-[0-9]+(-wal)?)\$" || true
}

# check_rendered [human-approved-exposure]: reads a rendered manifest on
# stdin and records a "fail-closed" check. Returns 1 when a default that
# must stay on is off; the caller stops with exit 4 for the human.
check_rendered() {
  local f bad="" n off
  f=$(cat)
  printf '%s\n' "$f" | grep -q '^kind: NetworkPolicy' || bad="$bad no NetworkPolicy (networkPolicy.enabled);"
  printf '%s\n' "$f" | grep -qE '^[[:space:]]*(privileged|hostNetwork|hostPID|hostIPC|allowPrivilegeEscalation): true' \
    && bad="$bad a privileged or host-namespace pod (security contexts);"
  n=$(printf '%s\n' "$f" | grep -c 'name: LANGSTROTH_RELAY_TUNNEL$' || true)
  off=$(printf '%s\n' "$f" | grep -A1 'name: LANGSTROTH_RELAY_TUNNEL$' | grep -c 'value: "off"' || true)
  if [ "$n" != "$off" ] && [ "${1:-0}" != 1 ]; then
    bad="$bad the tunnel is on (tunnel.mode: exposure is the human's decision; after they chose it, rerun with --human-approved-exposure);"
  fi
  if [ -n "$bad" ]; then
    check fail-closed fail "the values turn off fail-closed defaults:$bad" "stop and ask the human; they make and own that change"
    return 1
  fi
  check fail-closed pass "NetworkPolicy present, no privileged pod, tunnel off or approved"
}

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
