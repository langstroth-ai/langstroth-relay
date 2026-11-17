---
name: relay-run-local
description: Build and run the Ligus relay as a single local binary (no Kubernetes or Docker) with deploy/agent/local.sh, then verify, check status or stop it. Use when someone wants a personal or development relay on their own machine, or a relay for local testing of the Ligus app.
---

# Run the Ligus relay locally

`deploy/agent/local.sh` is non-interactive, idempotent and prints one JSON
object with `--json` (exit 0 ok, 1 failed with a `fix`, 2 usage, 3 missing
tool). Rules from AGENTS.md apply: never print the generated token, never
widen the bind address on your own.

## 1. Choose the data directory

Default: `$LIGUS_RELAY_DATA_DIR`, else `~/.local/state/ligus-relay`. For a
throwaway test use `mktemp -d`. The relay's store lives there
(`relay-state.json`); deleting it deletes every workspace's envelopes on this
relay, so only do that when the human says so.

## 2. Build (needs Go) and start

```sh
deploy/agent/local.sh build --json
deploy/agent/local.sh start --data-dir "$D" [--port 8443] --json
```

`start` generates a metrics token into `$D/secrets/` (mode 700, never
printed), starts the relay in the background on `127.0.0.1`, and waits for
`/v1/health`. Run again: it reports the running relay and changes nothing.
A port in use (OrbStack listens on 8443): pick another `--port`, for
example `--port 18443`, and use it in the URL you report.

## 3. Verify

```sh
deploy/agent/local.sh verify --data-dir "$D" --json
```

Passes when the process runs, `/v1/health` answers 200, `/metrics` refuses
requests without the token, and the bind is loopback. Tell the human the
relay URL (`http://127.0.0.1:<port>`) to enter in the Ligus app.

## 4. STOP: anything beyond this machine

Reaching the relay from other devices (phone, teammates) needs a non-loopback
bind, a firewall decision and TLS in front (Caddy, a tunnel, a reverse
proxy). Ask the human. Only after they choose, restart with
`--addr <address> --human-approved-exposure`. For a team, prefer the
Kubernetes skill (`relay-deploy-k8s`). The `deploy/compose/` stacks with an
identity provider need the human for domains, TLS and the OIDC client.

## 5. Stop

```sh
deploy/agent/local.sh stop --data-dir "$D" --json     # data kept
deploy/agent/local.sh status --data-dir "$D" --json   # exit 1 when stopped
```

Logs: `$D/run/ligus-relay.log`.
