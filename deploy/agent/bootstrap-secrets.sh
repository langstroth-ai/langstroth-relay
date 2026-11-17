#!/usr/bin/env bash
# Creates the Kubernetes Secrets a scenario needs, from fresh randomness,
# straight into the cluster. Idempotent: an existing Secret is left alone
# (delete it yourself to rotate). Never prints, logs or puts a secret value on
# a command line: values are written to files in a private temp directory,
# loaded with --from-file, and the directory is removed.
#
#   deploy/agent/bootstrap-secrets.sh --context orbstack --namespace ligus-relay \
#     --scenario team-bundled [--release ligus-relay] [--json]
#
# Values only a human can provide come from files the human wrote:
#   --dsn-file PATH                 team-external / auth-external: the full DSN
#   --idp-client-secret-file PATH   auth-*: the IdP OIDC client secret
#
# Also creates the namespace (labelled Pod Security "restricted") if absent.
# Exit: 0 done, 1 something is missing (see "fix"), 2 usage, 3 tool/cluster.
source "$(dirname "$0")/lib.sh"

usage() {
  cat >&2 <<EOF
usage: $0 --scenario S --context CTX [--namespace NS] [--release R]
          [--dsn-file PATH] [--idp-client-secret-file PATH] [--json]
  scenarios: $SCENARIOS
EOF
}

NS=${LIGUS_NAMESPACE:-}
SCENARIO=${LIGUS_SCENARIO:-}
RELEASE=${LIGUS_RELEASE:-}
DSN_FILE=""
IDP_FILE=""
while [ $# -gt 0 ]; do
  case $1 in
    --context) KCTX=${2:?}; shift 2 ;;
    --namespace) NS=${2:?}; shift 2 ;;
    --scenario) SCENARIO=${2:?}; shift 2 ;;
    --release) RELEASE=${2:?}; shift 2 ;;
    --dsn-file) DSN_FILE=${2:?}; shift 2 ;;
    --idp-client-secret-file) IDP_FILE=${2:?}; shift 2 ;;
    --json) JSON=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; die_usage "unknown argument $1" ;;
  esac
done
[ -n "$SCENARIO" ] || { usage; die_usage "--scenario is required"; }
valid_scenario "$SCENARIO" || die_usage "unknown scenario $SCENARIO (one of: $SCENARIOS)"
CHARTNAME=$(scenario_chartname "$SCENARIO")
NS=${NS:-$CHARTNAME}
RELEASE=${RELEASE:-$CHARTNAME}
FULL=$(fullname "$CHARTNAME" "$RELEASE")
DBMODE=$(scenario_dbmode "$SCENARIO")

require_context
valid --namespace "$NS" "$DNS1123"
valid --release "$RELEASE" "$DNS1123"
need_tool kubectl "install kubectl"
need_tool openssl "install openssl"
k get --raw /version >/dev/null 2>&1 || { log "cluster for context $KCTX unreachable"; exit 3; }

TMP=$(mktemp -d)
chmod 700 "$TMP"
trap 'rm -rf "$TMP"' EXIT
umask 077

RESULTS=()
PUBKEY=""
result() { # name status keys detail
  RESULTS+=("{\"name\":$(json_escape "$1"),\"status\":$(json_escape "$2"),\"keys\":$(json_escape "$3"),\"detail\":$(json_escape "$4")}")
  [ "$2" = needs-human ] && FAILED=1
  [ "$JSON" = 1 ] || log "[$2] secret $1 ($3) $4"
}
rand_hex() { openssl rand -hex "$1"; }
exists() { k -n "$NS" get secret "$1" >/dev/null 2>&1; }
LABELS=(app.kubernetes.io/managed-by=ligus-agent-bootstrap "ligus.ai/bootstrap-for=$FULL")
# create_secret NAME KEY=FILE...: from files only, then label.
create_secret() {
  local name=$1; shift
  local args=() kv
  for kv in "$@"; do args+=("--from-file=$kv"); done
  k -n "$NS" create secret generic "$name" "${args[@]}" >/dev/null
  k -n "$NS" label secret "$name" "${LABELS[@]}" --overwrite >/dev/null
}

# --- Namespace -----------------------------------------------------------------
if ! k get namespace "$NS" >/dev/null 2>&1; then
  k create namespace "$NS" >/dev/null
  k label namespace "$NS" pod-security.kubernetes.io/enforce=restricted \
    pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted \
    ligus.ai/created-by=bootstrap-secrets >/dev/null
  [ "$JSON" = 1 ] || log "[created] namespace $NS (pod-security restricted)"
  NS_STATUS=created
else
  NS_STATUS=exists
fi

# --- Relay settings directory (config.existingSecret) -------------------------
if [ "$CHARTNAME" = ligus-relay ]; then
  n="$FULL-env"
  if exists "$n"; then result "$n" exists LIGUS_RELAY_METRICS_TOKEN "left unchanged"
  else
    rand_hex 32 >"$TMP/metrics"
    create_secret "$n" "LIGUS_RELAY_METRICS_TOKEN=$TMP/metrics"
    result "$n" created LIGUS_RELAY_METRICS_TOKEN "bearer for /metrics"
  fi
fi

# --- ligus-auth issuer key and admin token -------------------------------------
if [ "$CHARTNAME" = ligus-auth ]; then
  n="$FULL-signing-key"
  if exists "$n"; then
    PUBKEY=$(k -n "$NS" get secret "$n" -o jsonpath='{.metadata.annotations.ligus\.ai/public-key}' 2>/dev/null || true)
    result "$n" exists signing-key "left unchanged"
  else
    if [ -n "${LIGUS_AUTH_BIN:-}" ]; then
      out=$("$LIGUS_AUTH_BIN" keygen --out "$TMP/signing-key")
    elif command -v go >/dev/null 2>&1; then
      out=$(cd "$REPO_ROOT" && go run ./cmd/ligus-auth keygen --out "$TMP/signing-key")
    else
      log "need go (or LIGUS_AUTH_BIN pointing at a ligus-auth binary) to generate the issuer key"; exit 3
    fi
    PUBKEY=$(printf '%s\n' "$out" | sed -n 's/^public_key=//p')
    create_secret "$n" "signing-key=$TMP/signing-key"
    k -n "$NS" annotate secret "$n" "ligus.ai/public-key=$PUBKEY" --overwrite >/dev/null
    result "$n" created signing-key "Ed25519 issuer seed; public key in .public_key"
  fi
  n="$FULL-admin"
  if exists "$n"; then result "$n" exists admin-token "left unchanged"
  else
    rand_hex 32 >"$TMP/admin"
    create_secret "$n" "admin-token=$TMP/admin"
    result "$n" created admin-token "admin API bearer (port-forward only)"
  fi
  n="$FULL-secrets"
  if exists "$n"; then result "$n" exists idp-client-secret "left unchanged"
  elif [ -n "$IDP_FILE" ]; then
    [ -r "$IDP_FILE" ] || die_usage "--idp-client-secret-file $IDP_FILE is not readable"
    create_secret "$n" "idp-client-secret=$IDP_FILE"
    result "$n" created idp-client-secret "from the human's file"
  else
    result "$n" needs-human idp-client-secret "the human creates the OIDC client at the IdP and writes its secret to a file; rerun with --idp-client-secret-file FILE"
  fi
fi

# --- Database ---------------------------------------------------------------------
role=relay
[ "$CHARTNAME" = ligus-auth ] && role=ligus_auth
case $DBMODE in
  bundled)
    n="$FULL-postgres-auth"
    if exists "$n"; then result "$n" exists "password,pgpass" "left unchanged"
    else
      pw=$(rand_hex 24)
      printf '%s' "$pw" >"$TMP/password"
      printf '*:*:*:%s:%s' "$role" "$pw" >"$TMP/pgpass"
      unset pw
      create_secret "$n" "password=$TMP/password" "pgpass=$TMP/pgpass"
      result "$n" created "password,pgpass" "role $role"
    fi
    n="$FULL-postgres-tls"
    if exists "$n"; then result "$n" exists "tls.crt,tls.key,ca.crt" "left unchanged"
    else
      host="$FULL-postgres"
      openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 825 \
        -subj "/CN=$host-ca" -keyout "$TMP/ca.key" -out "$TMP/ca.crt" >/dev/null 2>&1
      openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=$host" \
        -keyout "$TMP/tls.key" -out "$TMP/tls.csr" >/dev/null 2>&1
      printf 'subjectAltName=DNS:%s,DNS:%s.%s,DNS:%s.%s.svc,DNS:%s.%s.svc.cluster.local\nextendedKeyUsage=serverAuth\n' \
        "$host" "$host" "$NS" "$host" "$NS" "$host" "$NS" >"$TMP/ext"
      openssl x509 -req -in "$TMP/tls.csr" -CA "$TMP/ca.crt" -CAkey "$TMP/ca.key" -CAcreateserial \
        -days 825 -extfile "$TMP/ext" -out "$TMP/tls.crt" >/dev/null 2>&1
      create_secret "$n" "tls.crt=$TMP/tls.crt" "tls.key=$TMP/tls.key" "ca.crt=$TMP/ca.crt"
      result "$n" created "tls.crt,tls.key,ca.crt" "server cert for $host from a per-release CA (CA key discarded; valid 825 days; or switch to cert-manager)"
    fi
    ;;
  external)
    n="$FULL-database"
    if exists "$n"; then result "$n" exists DATABASE_URL "left unchanged"
    elif [ -n "$DSN_FILE" ]; then
      [ -r "$DSN_FILE" ] || die_usage "--dsn-file $DSN_FILE is not readable"
      grep -q 'sslmode=verify-full' "$DSN_FILE" || log "warning: the DSN does not say sslmode=verify-full; the chart expects verified TLS to an external database"
      create_secret "$n" "DATABASE_URL=$DSN_FILE"
      result "$n" created DATABASE_URL "from the human's file"
    else
      result "$n" needs-human DATABASE_URL "the human writes the DSN (sslmode=verify-full) to a file outside the repo; rerun with --dsn-file FILE"
    fi
    ;;
  cnpg) : ;; # the operator creates the role's credentials (<fullname>-db-app)
  none) : ;;
esac

if [ "$JSON" = 1 ]; then
  ok=true; [ "$FAILED" = 1 ] && ok=false
  IFS=,
  printf '{"tool":"bootstrap-secrets","ok":%s,"context":%s,"namespace":%s,"namespace_status":%s,"scenario":%s,"release":%s,"fullname":%s,"public_key":%s,"secrets":[%s]}\n' \
    "$ok" "$(json_escape "$KCTX")" "$(json_escape "$NS")" "$(json_escape "$NS_STATUS")" "$(json_escape "$SCENARIO")" \
    "$(json_escape "$RELEASE")" "$(json_escape "$FULL")" "$(json_escape "$PUBKEY")" "${RESULTS[*]-}"
  unset IFS
elif [ -n "$PUBKEY" ]; then
  log "issuer public key (not secret; LIGUS_RELAY_TOKEN_PUBKEY on the relay): $PUBKEY"
fi
exit "$FAILED"
