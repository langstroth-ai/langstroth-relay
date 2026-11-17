---
name: relay-deploy-k8s
description: Deploy, verify, upgrade or remove the Ligus relay (ligus-relay) and the optional ligus-auth sign-in service on Kubernetes with the non-interactive scripts in deploy/agent. Use when asked to install the relay on a cluster (kind, k3d, OrbStack, EKS, GKE, ...), pick a database mode (bundled, CloudNativePG, external), check a cluster is ready, or tear an install down.
---

# Deploy the Ligus relay on Kubernetes

Follow these steps in order. Every script takes `--json` and prints one
object with `ok` and `checks[]`; on `ok:false` read each failed check's
`fix` and do that, or stop and ask the human where it says so. Rules from
AGENTS.md apply throughout: no secret values anywhere you write or print;
never relax NetworkPolicy, Pod Security, TLS, tunnel-off or backups to get
past a check.

## 1. STOP: agree the target with the human

Ask, and do not guess:
- **Which kube context and namespace?** (`kubectl config get-contexts -o name`
  lists them; do not just use the current one.)
- **Which scenario?** Show the AGENTS.md decision table. Default:
  `team-bundled` for a team, `eval-memory` to try it.
- For `team-external`: the human writes the DSN (with `sslmode=verify-full`)
  to a file outside the repo (mode 600) and gives you its path, plus the
  database address range for `database.external.cidrs`.
- For `team-cnpg`: the object store for backups and the name of the Secret
  holding its credentials (the human creates that Secret).
- For `auth-*`: the human fills in a copy of
  `deploy/agent/values/auth-example-human.yaml` (public URL, IdP issuer,
  client id, tenants, grants, ingress) and writes the IdP client secret to a
  file.

Set `C`, `NS`, `S` (and `R`, default `ligus-relay` / `ligus-auth`).

## 2. Preflight (read-only)

```sh
deploy/agent/preflight.sh --context $C --namespace $NS --scenario $S --probe --json
```

- exit 3: tooling or cluster access. Fix tools yourself; credentials are the human's.
- `networkpolicy` fail: the CNI does not enforce policy. **STOP**, tell the
  human; do not disable the policy.
- `cnpg-operator` fail on a cnpg scenario: **STOP** and ask whether to
  install the cluster-wide operator. Only on a yes:
  `deploy/agent/cnpg-operator.sh install --context $C --json`
- `storage-class` / `storage-rwop` fail: ask the human for a StorageClass;
  pass it in an extra values file.
- `pod-security` warn on an existing namespace: label it restricted if the
  namespace is yours, otherwise ask.

## 3. Secrets (idempotent, values never shown)

```sh
deploy/agent/bootstrap-secrets.sh --context $C --namespace $NS --scenario $S --release $R --json \
  [--dsn-file PATH] [--idp-client-secret-file PATH]
```

Entries with status `needs-human` mean a file from step 1 is missing. For
`auth-*`, the output's `public_key` (not secret) is the issuer key the relay
needs as `LIGUS_RELAY_TOKEN_PUBKEY`.

## 4. Dry run, then install

```sh
deploy/agent/install.sh --context $C --namespace $NS --scenario $S --release $R [--values FILE ...] --dry-run --json
deploy/agent/install.sh --context $C --namespace $NS --scenario $S --release $R [--values FILE ...] --json
```

Use `--build-image` on a local cluster without access to the published
images, or `--image-repo/--image-tag`. If the chart refuses a value, its
message names the setting: fix the input, never the safety check.

## 5. Verify

```sh
deploy/agent/verify.sh --context $C --namespace $NS --release $R --json
```

All checks `pass` (or `skip`) means done. Report to the human: the namespace,
the release, the in-cluster URL (`http://<release>-sync.<ns>.svc:8443`, or
`http://<release>.<ns>.svc:8443` for `eval-memory`), and that nothing is
exposed outside the cluster yet.

## 6. STOP: exposure is the human's

Ingress or HTTPRoute host, DNS, TLS, and `clientIP.trustedProxies` are
human decisions. Once given, put them in a values file and rerun step 4.
Same for turning on the tunnel (`tunnel.mode`) or push.

## Upgrade, roll back, remove

- Upgrade: rerun step 4 (dry run first) and step 5.
- Roll back: `helm --kube-context $C -n $NS history $R`, then
  `helm --kube-context $C -n $NS rollback $R <rev>` and step 5.
- Remove: `deploy/agent/teardown.sh --context $C --namespace $NS --release $R --json`
  keeps data and Secrets. Add `--delete-data`, `--delete-secrets` or
  `--delete-namespace` **only** after the human says the data can go. Remove
  the operator (`cnpg-operator.sh uninstall`) only if this workflow installed it
  and the human agrees.
