#!/usr/bin/env bash
# Rename a Fly relay app's secrets from HIVE_RELAY_* / LIGUS_RELAY_* to
# LANGSTROTH_RELAY_* before deploying v0.3.0, which no longer reads the old
# names and refuses to start while one is set.
#
#   deploy/fly-rename-secrets.sh <app>            # plan only: names, no changes
#   deploy/fly-rename-secrets.sh <app> --apply    # stage the renames
#
# Then deploy v0.3.0 (the deploy applies the staged secrets) and check
# `fly logs` for the tunnel/push lines.
#
# No secret value is ever printed or put on a command line. Each value is read
# from the running machine (`fly ssh console -C 'printenv NAME'`) into a
# mode-0600 temp file, staged under its new name with `fly secrets import
# --stage` from that file, and checked by comparing the digests `fly secrets
# list` shows for the old and the new name. Only then are the old names
# unset (also staged). Nothing restarts until the next deploy, so the running
# v0.2 relay keeps its old secrets until v0.3.0 replaces it.
#
# Rules:
#   - HIVE_RELAY_<X> and LIGUS_RELAY_<X> both set: the LIGUS_RELAY_ value is
#     kept (the one the running relay uses); the HIVE_RELAY_ one is dropped.
#   - LANGSTROTH_RELAY_<X> already set: it is kept (the running relay already
#     prefers it); the old names are only unset.
#   - Settings no longer read at all (DROP below) are unset, not renamed.
set -euo pipefail

FLY=${FLYCTL:-flyctl}
# Settings the open relay does not read under any name. ADMIN_LOGINS was read
# only by the old Rust relay and the enterprise build.
DROP=" ADMIN_LOGINS "

APP=${1:-}
MODE=${2:-plan}
[ -n "$APP" ] && [[ "$APP" =~ ^[a-z0-9-]+$ ]] || { echo "usage: $0 <app> [--apply]" >&2; exit 2; }
case "$MODE" in plan|--plan) MODE=plan ;; --apply) MODE=apply ;; *) echo "usage: $0 <app> [--apply]" >&2; exit 2 ;; esac
command -v jq >/dev/null || { echo "need jq" >&2; exit 3; }

# name<TAB>digest for every secret of the app.
list() { "$FLY" secrets list --app "$APP" --json | jq -r '.[] | [(.Name // .name), (.Digest // .digest // "")] | @tsv'; }
digest_of() { awk -F'\t' -v n="$1" '$1 == n { print $2 }' <<<"$LIST"; }
has() { awk -F'\t' -v n="$1" '$1 == n { f = 1 } END { exit !f }' <<<"$LIST"; }

LIST=$(list)
declare -a COPY_FROM=() COPY_TO=() UNSET=()
OLD=()
while IFS= read -r n; do [ -n "$n" ] && OLD+=("$n"); done < <(cut -f1 <<<"$LIST" | grep -E '^(HIVE|LIGUS)_RELAY_' | sort -u || true)
for name in ${OLD[@]+"${OLD[@]}"}; do
  short=${name#HIVE_RELAY_}; short=${short#LIGUS_RELAY_}
  new=LANGSTROTH_RELAY_$short
  UNSET+=("$name")
  if [[ "$DROP" == *" $short "* ]]; then
    echo "drop    $name (not read by the relay)"
  elif has "$new"; then
    # The running relay refuses two different values for one setting, so a
    # differing digest means one of them was changed by hand: stop.
    if [ "$(digest_of "$new")" != "$(digest_of "$name")" ]; then
      echo "$new is already set to a different value than $name; resolve it by hand (keep the value in use), then rerun" >&2
      exit 1
    fi
    echo "keep    $new (already set, same digest); unset $name"
  elif [[ $name == HIVE_RELAY_* ]] && has "LIGUS_RELAY_$short"; then
    echo "drop    $name (LIGUS_RELAY_$short is the value in use)"
  else
    echo "rename  $name -> $new"
    COPY_FROM+=("$name"); COPY_TO+=("$new")
  fi
done
if [ ${#UNSET[@]} -eq 0 ]; then echo "no HIVE_RELAY_* or LIGUS_RELAY_* secrets on $APP"; exit 0; fi
if [ "$MODE" = plan ]; then echo "plan only; rerun with --apply to stage these changes"; exit 0; fi

TMP=$(mktemp -d); chmod 700 "$TMP"
trap 'rm -rf "$TMP"' EXIT
umask 077
: >"$TMP/import"
for ((i = 0; i < ${#COPY_FROM[@]}; i++)); do
  from=${COPY_FROM[$i]} to=${COPY_TO[$i]}
  # The machine's environment holds the deployed secret values.
  "$FLY" ssh console --app "$APP" --quiet -C "printenv $from" >"$TMP/v" </dev/null
  # Drop the trailing newline printenv adds (and a CR if a pty added one).
  v=$(cat "$TMP/v"); v=${v%$'\r'}
  [ -n "$v" ] || { echo "could not read $from from the running machine; nothing was changed" >&2; exit 1; }
  if [[ "$v" == *$'\n'* ]]; then
    printf '%s="""\n%s\n"""\n' "$to" "$v" >>"$TMP/import"
  else
    printf '%s=%s\n' "$to" "$v" >>"$TMP/import"
  fi
  v=
done
rm -f "$TMP/v"
if [ ${#COPY_FROM[@]} -gt 0 ]; then
  "$FLY" secrets import --app "$APP" --stage <"$TMP/import" >/dev/null
fi
rm -f "$TMP/import"

# Each new name must hold exactly the old value (same digest) before any old
# name goes away.
LIST=$(list)
for ((i = 0; i < ${#COPY_FROM[@]}; i++)); do
  from=${COPY_FROM[$i]} to=${COPY_TO[$i]}
  a=$(digest_of "$from") b=$(digest_of "$to")
  if [ -z "$b" ] || [ "$a" != "$b" ]; then
    echo "digest of $to does not match $from; the old names are left in place. Fix $to (fly secrets set --stage) and rerun." >&2
    exit 1
  fi
  echo "ok      $to (digest matches $from)"
done
"$FLY" secrets unset --app "$APP" --stage "${UNSET[@]}" >/dev/null
echo "staged: unset ${UNSET[*]}"
echo "Now deploy v0.3.0; the staged secrets apply with it."
