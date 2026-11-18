#!/usr/bin/env bash
# Runs the relay as a single local binary (no Kubernetes, no Docker): a
# personal relay on a laptop or home server. Memory store snapshotted to a
# data directory; bound to 127.0.0.1 unless the human chooses otherwise.
#
#   deploy/agent/local.sh build  [--bin PATH]
#   deploy/agent/local.sh start  [--data-dir DIR] [--port 8443] [--addr 127.0.0.1] [--bin PATH]
#                                [--human-approved-exposure]  (needed for a non-loopback --addr)
#   deploy/agent/local.sh status [--data-dir DIR]
#   deploy/agent/local.sh verify [--data-dir DIR]
#   deploy/agent/local.sh stop   [--data-dir DIR]
#   (every action takes --json)
#
# Layout under the data directory (default $LANGSTROTH_RELAY_DATA_DIR, else
# ~/.local/state/langstroth-relay):
#   relay-state.json        the relay's snapshot (the relay writes it)
#   secrets/                LANGSTROTH_RELAY_SECRETS_DIR (mode 700): a generated
#                           LANGSTROTH_RELAY_METRICS_TOKEN; values never printed
#   run/langstroth-relay.pid     the running process
#   run/langstroth-relay.log     its log
#   run/addr                the address it was started on
#
# Idempotent: start on a running relay reports it and changes nothing; stop
# on a stopped one is a no-op. Exit: 0 ok, 1 failed (see "fix"), 2 usage,
# 3 a tool is missing.
source "$(dirname "$0")/lib.sh"

usage() { awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$0" >&2; }
ACTION=${1:-}
[ $# -gt 0 ] && shift
STATE_HOME=${XDG_STATE_HOME:-$HOME/.local/state}
DEFAULT_DATA_DIR=$STATE_HOME/langstroth-relay
# A data directory made before the Langstroth rename keeps being used.
if [ ! -e "$DEFAULT_DATA_DIR" ] && [ -d "$STATE_HOME/ligus-relay" ]; then DEFAULT_DATA_DIR=$STATE_HOME/ligus-relay; fi
# The LIGUS_RELAY_* names from before the rename are no longer read.
for v in LIGUS_RELAY_DATA_DIR LIGUS_RELAY_LOCAL_PORT LIGUS_RELAY_LOCAL_ADDR LIGUS_RELAY_BIN; do
  [ -z "${!v+x}" ] || die_usage "$v is no longer read: use LANGSTROTH_${v#LIGUS_}"
done
DATA_DIR=${LANGSTROTH_RELAY_DATA_DIR:-$DEFAULT_DATA_DIR}
PORT=${LANGSTROTH_RELAY_LOCAL_PORT:-8443}
ADDR=${LANGSTROTH_RELAY_LOCAL_ADDR:-127.0.0.1}
BIN=${LANGSTROTH_RELAY_BIN:-$REPO_ROOT/bin/langstroth-relay}
EXPOSE=0
while [ $# -gt 0 ]; do
  case $1 in
    --data-dir) DATA_DIR=${2:?}; shift 2 ;;
    --port) PORT=${2:?}; shift 2 ;;
    --addr) ADDR=${2:?}; shift 2 ;;
    --bin) BIN=${2:?}; shift 2 ;;
    --human-approved-exposure) EXPOSE=1; shift ;;
    --json) JSON=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die_usage "unknown argument $1" ;;
  esac
done
case $ACTION in build|start|status|verify|stop) ;; -h|--help) usage; exit 0 ;; *) usage; die_usage "action must be build, start, status, verify or stop" ;; esac
case $PORT in ''|*[!0-9]*) die_usage "--port must be a number" ;; esac
RUN="$DATA_DIR/run"
PIDF="$RUN/langstroth-relay.pid"
# A relay started before the Langstroth rename wrote run/ligus-relay.pid.
if [ ! -e "$PIDF" ] && [ -e "$RUN/ligus-relay.pid" ]; then PIDF="$RUN/ligus-relay.pid"; fi

emit() {
  if [ "$JSON" = 1 ]; then
    local ok=true; [ "$FAILED" = 1 ] && ok=false
    printf '{"tool":"local","action":%s,"ok":%s,"data_dir":%s,"url":%s,"log":%s,"checks":%s}\n' \
      "$(json_escape "$ACTION")" "$ok" "$(json_escape "$DATA_DIR")" "$(json_escape "${URL-}")" \
      "$(json_escape "$RUN/langstroth-relay.log")" "$(checks_json)"
  fi
  exit "$FAILED"
}
running_pid() { # prints the pid when our relay is alive
  [ -r "$PIDF" ] || return 1
  local p; p=$(cat "$PIDF")
  case $p in ''|*[!0-9]*) return 1 ;; esac
  kill -0 "$p" 2>/dev/null || return 1
  ps -p "$p" -o command= 2>/dev/null | grep -Eq 'langstroth-relay|ligus-relay' || return 1
  printf '%s' "$p"
}
url_of() { if [ -r "$RUN/addr" ]; then printf 'http://%s' "$(cat "$RUN/addr")"; fi; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@" || true; }

case $ACTION in
  build)
    need_tool go "install Go (see go.mod for the version), or download a release binary and pass --bin"
    mkdir -p "$(dirname "$BIN")"
    if (cd "$REPO_ROOT" && go build -trimpath -o "$BIN" ./cmd/langstroth-relay) >&2; then
      check build pass "built $BIN"
    else
      check build fail "go build failed" "read the compiler output above"
    fi
    URL=""; emit ;;

  start)
    URL="http://$ADDR:$PORT"
    if p=$(running_pid); then
      URL=$(url_of); check start pass "already running (pid $p) at $URL; left unchanged"; emit
    fi
    # Loopback means a literal 127.x.y.z, localhost or ::1: a host name that
    # merely starts with "127." can resolve anywhere.
    if printf '%s' "$ADDR" | grep -Eq '^(127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|localhost|::1)$'; then loop=1; else loop=0; fi
    case $loop in
      1) ;;
      *) [ "$EXPOSE" = 1 ] || { check exposure fail "--addr $ADDR would expose the relay beyond this machine" \
           "exposure (bind address, TLS, firewall) is the human's decision; rerun with --addr only after they chose it, and put TLS in front, passing --human-approved-exposure"
         emit; } ;;
    esac
    [ -x "$BIN" ] || { check binary fail "$BIN missing" "deploy/agent/local.sh build (or pass --bin)"; emit; }
    need_tool curl "install curl"
    need_tool openssl "install openssl"
    # Anything accepting connections counts (not only HTTP): otherwise the
    # bind fails later, or the health check reaches someone else's server.
    if (exec 3<>"/dev/tcp/$ADDR/$PORT") 2>/dev/null; then
      check port fail "something already listens on $ADDR:$PORT" "pick another --port (do not stop a process you did not start)"; emit
    fi
    umask 077
    mkdir -p "$DATA_DIR" "$RUN" "$DATA_DIR/secrets"
    chmod 700 "$DATA_DIR" "$DATA_DIR/secrets"
    # A data directory from before the Langstroth rename has the token as
    # LIGUS_RELAY_METRICS_TOKEN, a name the relay no longer reads and refuses
    # to start with: rename it (or drop it when the new name exists).
    if [ -e "$DATA_DIR/secrets/LIGUS_RELAY_METRICS_TOKEN" ]; then
      if [ -s "$DATA_DIR/secrets/LANGSTROTH_RELAY_METRICS_TOKEN" ]; then
        rm -f "$DATA_DIR/secrets/LIGUS_RELAY_METRICS_TOKEN"
      else
        mv "$DATA_DIR/secrets/LIGUS_RELAY_METRICS_TOKEN" "$DATA_DIR/secrets/LANGSTROTH_RELAY_METRICS_TOKEN"
      fi
      check metrics-token-name pass "renamed secrets/LIGUS_RELAY_METRICS_TOKEN to LANGSTROTH_RELAY_METRICS_TOKEN"
    fi
    if [ ! -s "$DATA_DIR/secrets/LANGSTROTH_RELAY_METRICS_TOKEN" ]; then
      openssl rand -hex 32 >"$DATA_DIR/secrets/LANGSTROTH_RELAY_METRICS_TOKEN"
      check metrics-token pass "generated secrets/LANGSTROTH_RELAY_METRICS_TOKEN (not printed)"
    else
      check metrics-token pass "kept the existing metrics token"
    fi
    # Only the variables this script sets reach the relay's settings; any
    # other LANGSTROTH_RELAY_* / DATABASE_URL in the caller's environment is dropped
    # so a stray variable can't silently change the relay.
    env -i PATH="$PATH" HOME="$HOME" \
      LANGSTROTH_RELAY_ADDR="$ADDR:$PORT" LANGSTROTH_RELAY_DATA_DIR="$DATA_DIR" \
      LANGSTROTH_RELAY_SECRETS_DIR="$DATA_DIR/secrets" \
      nohup "$BIN" >>"$RUN/langstroth-relay.log" 2>&1 </dev/null &
    echo $! >"$PIDF"
    printf '%s:%s' "$ADDR" "$PORT" >"$RUN/addr"
    for _ in $(seq 40); do
      [ "$(code "$URL/v1/health")" = 200 ] && break
      kill -0 "$(cat "$PIDF")" 2>/dev/null || break
      sleep 0.25
    done
    if [ "$(code "$URL/v1/health")" = 200 ]; then
      check start pass "running (pid $(cat "$PIDF")) at $URL"
    else
      kill "$(cat "$PIDF")" 2>/dev/null || true
      rm -f "$PIDF" "$RUN/addr"
      check start fail "the relay did not answer /v1/health: $(tail -n 1 "$RUN/langstroth-relay.log" 2>/dev/null)" "read $RUN/langstroth-relay.log; on 'address already in use' pick another --port"
    fi
    emit ;;

  status)
    URL=$(url_of)
    if p=$(running_pid); then check running pass "pid $p at $URL"
    else check running fail "not running" "deploy/agent/local.sh start --data-dir $DATA_DIR"; fi
    emit ;;

  verify)
    need_tool curl "install curl"
    URL=$(url_of)
    if ! p=$(running_pid); then check running fail "not running" "deploy/agent/local.sh start --data-dir $DATA_DIR"; emit; fi
    check running pass "pid $p"
    c=$(code "$URL/v1/health")
    if [ "$c" = 200 ]; then check health pass "$URL/v1/health 200"; else check health fail "$URL/v1/health $c" "tail -n 50 $RUN/langstroth-relay.log"; fi
    c=$(code "$URL/metrics")
    if [ "$c" = 200 ]; then
      check metrics-gated fail "/metrics answers without a token" "check secrets/LANGSTROTH_RELAY_METRICS_TOKEN exists and restart"
    else
      TMPH=$(mktemp); chmod 600 "$TMPH"
      tokf="$DATA_DIR/secrets/LANGSTROTH_RELAY_METRICS_TOKEN"
      { printf 'Authorization: Bearer '; cat "$tokf"; printf '\n'; } >"$TMPH"
      c2=$(code -H @"$TMPH" "$URL/metrics"); rm -f "$TMPH"
      if [ "$c2" = 200 ]; then check metrics-gated pass "/metrics $c without the token, 200 with it"
      else check metrics-gated fail "/metrics $c2 with the token" "restart: deploy/agent/local.sh stop; deploy/agent/local.sh start"; fi
    fi
    case $(cat "$RUN/addr") in
      127.*|localhost*|\[::1\]*) check exposure pass "bound to loopback only" ;;
      *) check exposure warn "bound to $(cat "$RUN/addr")" "make sure the human chose this and TLS sits in front" ;;
    esac
    emit ;;

  stop)
    URL=$(url_of)
    if p=$(running_pid); then
      kill "$p" 2>/dev/null || true
      for _ in $(seq 40); do kill -0 "$p" 2>/dev/null || break; sleep 0.25; done
      if kill -0 "$p" 2>/dev/null; then check stop fail "pid $p still running" "kill $p, then rerun stop"
      else rm -f "$PIDF"; check stop pass "stopped pid $p; data kept in $DATA_DIR (delete it yourself only if the human says so)"; fi
    else
      rm -f "$PIDF"; check stop pass "not running"
    fi
    emit ;;
esac
