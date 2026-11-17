# AGENTS.md: deploying the Ligus relay

For coding agents (Claude Code, Codex, Copilot CLI, ...) and the humans who
drive them. Ligus (LEE-gus) is short for *ligustica*, the Italian honeybee.
This repository holds the relay (`ligus-relay`) and the optional sign-in
service (`ligus-auth`), their Helm charts (`deploy/helm/`) and the agent
tooling in `deploy/agent/`. Step-by-step skills: `.claude/skills/relay-deploy-k8s`
and `.claude/skills/relay-run-local` (plain Markdown; any agent can follow them).

## Pick a deployment

| Goal | Run | Database | Human input needed |
|---|---|---|---|
| Try it / one person, one machine | `deploy/agent/local.sh` (binary) | memory + snapshot in a data dir | none (loopback only) |
| Try it on a laptop cluster (kind, k3d, OrbStack) | scenario `eval-memory` | memory + snapshot on a PVC | none |
| Small team on Kubernetes, no managed Postgres | scenario `team-bundled` | chart's single hardened Postgres pod | exposure (ingress, DNS, TLS) |
| Team with a managed/existing Postgres | scenario `team-external` | external, `sslmode=verify-full` | DSN file, database CIDRs, exposure |
| Production-shaped, HA database | scenario `team-cnpg` | CloudNativePG, 3 instances, backups | operator install, backup bucket + credentials, exposure |
| Evaluate the CloudNativePG path | scenario `team-cnpg-eval` | CNPG, 1 instance, backups OFF | operator install |
| Sign-in with your IdP in front of a relay | scenario `auth-file` / `auth-bundled` / `auth-cnpg-eval` / `auth-external` (chart `ligus-auth`) | as named | IdP client + secret, public URL, tenants, exposure |

Not sure? Ask the human. Default to `local.sh` for one person and
`team-bundled` for a team on Kubernetes.

## Tooling contract (all of `deploy/agent/*.sh`)

- Non-interactive: flags or environment only; never a prompt.
- Idempotent: rerunning leaves the same state (existing Secrets are never
  changed; `install` is `helm upgrade --install`).
- `--json` prints exactly one JSON object on stdout:
  `{"tool":..., "ok":bool, ..., "checks":[{"name","status","detail","fix"}]}`
  with `status` one of `pass`, `warn`, `fail`, `skip`. Logs go to stderr.
  A failed check always says what to do next in `fix`.
- Exit codes: `0` ok, `1` a check or step failed (read `fix`), `2` usage
  error, `3` a tool is missing or the cluster is unreachable.
- Always pass `--context` explicitly; the scripts never act on "whatever is
  current" silently.

## Kubernetes: exact commands

```sh
C=<kube context the human named>  NS=ligus-relay  S=team-bundled
deploy/agent/preflight.sh         --context $C --namespace $NS --scenario $S --probe --json
deploy/agent/bootstrap-secrets.sh --context $C --namespace $NS --scenario $S --json
deploy/agent/install.sh           --context $C --namespace $NS --scenario $S --dry-run --json
deploy/agent/install.sh           --context $C --namespace $NS --scenario $S --json
deploy/agent/verify.sh            --context $C --namespace $NS --release ligus-relay --json
```

- **preflight** is read-only (with `--probe` it creates and deletes one
  `ligus-preflight-*` namespace to prove NetworkPolicy enforcement and
  ReadWriteOncePod). Checks: Helm >= 3.14, context, reachability, server
  version, RBAC, Pod Security, DNS selector, NetworkPolicy, StorageClass,
  RWOP, CNPG operator (cnpg scenarios), ingress classes, openssl.
- **bootstrap-secrets** creates the namespace (Pod Security `restricted`) and
  the scenario's Secrets from fresh randomness: the relay's metrics token,
  the bundled Postgres password and a per-release TLS CA, the ligus-auth
  issuer key and admin token. Values go file to Secret and are never printed.
  Inputs only a human has come from files they wrote: `--dsn-file`,
  `--idp-client-secret-file`. Missing ones are reported as `needs-human`.
- **install** runs preflight, checks the Secrets and the human's values
  files, renders (`helm template`), server-side dry-runs, then
  `helm upgrade --install --wait`. Extra settings: `--values FILE` (repeat).
  Images: `--image-repo/--image-tag`, or `--build-image` to build from this
  checkout (kind/k3d load it; OrbStack and Docker Desktop share the image store).
- **verify**: release deployed, pods ready, CNPG cluster healthy, `helm test`,
  `/v1/health` (or `/healthz`) through the Service, `/metrics` refused
  without its token, no Secret in a pod environment, NetworkPolicies present,
  Pod Security `restricted`.
- **CNPG operator** (cluster-wide; the human decides):
  `deploy/agent/cnpg-operator.sh status|install|uninstall --context $C --json`.
  `install` is a no-op when CNPG exists; `uninstall` only removes an operator
  this script installed and refuses while any CNPG cluster remains.

### Roll back or uninstall

```sh
helm --kube-context $C -n $NS history ligus-relay
helm --kube-context $C -n $NS rollback ligus-relay <revision>
deploy/agent/teardown.sh --context $C --namespace $NS --release ligus-relay --json
#   keeps PVCs and bootstrap Secrets; add --delete-data / --delete-secrets /
#   --delete-namespace only when the human said the data can go.
#   --delete-namespace only deletes a namespace bootstrap-secrets.sh created.
```

## Local: exact commands

```sh
D=${LIGUS_RELAY_DATA_DIR:-$HOME/.local/state/ligus-relay}
deploy/agent/local.sh build  --json                 # go build -> bin/ligus-relay
deploy/agent/local.sh start  --data-dir $D --json   # 127.0.0.1:8443, background
deploy/agent/local.sh verify --data-dir $D --json   # health, /metrics gated, loopback
deploy/agent/local.sh stop   --data-dir $D --json   # data kept
```

The data dir holds `relay-state.json` (the store), `secrets/` (mode 700,
read through `LIGUS_RELAY_SECRETS_DIR`) and `run/` (pid, log). Clients use
`http://127.0.0.1:8443`. A non-loopback `--addr` is refused unless the human
chose it (`--human-approved-exposure`); put TLS in front. The
`deploy/compose/` stacks (relay + ligus-auth + an IdP behind Caddy) are
examples that need a human throughout (domains, TLS, IdP client).

## Hard rules for agents

1. **No secret in chat, logs, command lines or values files.** That includes
   tokens, DSNs, passwords, private keys, invites and workspace keys. Create
   them with `bootstrap-secrets.sh` or `local.sh`; a value only the human
   has goes in a file they write (mode 600, outside the repo) and you pass
   the *path*. Never `kubectl get secret -o yaml`, `--set password=...`,
   `echo $TOKEN`, or paste a DSN. The issuer *public* key is not secret.
2. **Never weaken a fail-closed default to make an install pass**:
   `networkPolicy.enabled`, Pod Security `restricted`, the security
   contexts, TLS to the database (`verify-full`), `tunnel.mode=off`, the
   metrics token, backups on `team-cnpg`, `allowPlaintextInNamespace`,
   loopback-only local binds. When a check or the chart refuses, stop and
   ask the human; show them the `fix`.
3. **Always name the cluster** (`--context`) and confirm it with the human
   before the first change. Never touch namespaces you did not create.
4. **Never delete data** (`--delete-data`, `--delete-namespace`, a data dir,
   the CNPG operator) without the human saying so.

## Steps that need a human

- Choosing the cluster, namespace and scenario; cloud credentials and
  kubeconfig access.
- Exposure: ingress/Gateway host, DNS, TLS certificates,
  `clientIP.trustedProxies` (relay) or `config.trusted_proxies` (ligus-auth), any non-loopback local bind.
- Installing the cluster-wide CloudNativePG operator.
- `team-external`: the DSN file and the database CIDRs.
- `team-cnpg`: the backup object store and its credentials Secret.
- `ligus-auth`: registering the OIDC client at the IdP, its client-secret
  file, the public URL, tenants and group grants, and pointing the relay at
  the issuer public key (`LIGUS_RELAY_TOKEN_PUBKEY`). Passkey sign-in at the
  IdP is the human's.
- Turning on the tunnel (`tunnel.mode`, allowed hosts) and APNs push keys.
- Workspace invites and device approval happen in the Ligus app, not here.

## Developing

`go test ./...` (includes `agent_deploy_test.go`: shellcheck, exit codes,
the `--json` contract, scenario renders), `go vet ./...`,
`helm lint --strict deploy/helm/ligus-relay` (and each `ci/*-values.yaml`).
