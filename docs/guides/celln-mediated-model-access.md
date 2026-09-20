# Mediated model access for native Celln Agents

With mediation on, a native Celln Agent's model requests leave the node for the
**model gateway**, which adds that Agent's own provider key from the Secret its
`ModelConnection` names in the Agent's namespace. Celln nodes hold no provider
key for such runs. The controller signs a short-lived permit per run; the node
dispatcher's *scoped receiver* and the gateway both verify it against the same
public keyset.

It is **off by default**. With `celln.mediation.enabled=false` the chart renders
exactly what it rendered before this feature existed.

Mediation leaves the fleet's own configuration alone: `celln.fleet.backends`,
the host profiles and `celln.fleet.modelCredentialsSecret` are rendered and
configured on every node exactly as before, and the two paths run side by side
on one controller. Each run takes the path of its resolved model route
(`internal/controller/celln_scoped.go`):

- a connection with a host `credentialProfile` (a fleet backend, policy auth
  `host-profile`) starts as a fleet parent exactly as it does today, and its
  follow-up turns go to that parent;
- an Agent's own connection with a `secretRef` (policy auth `secret`), or a
  credential-free one, goes through the scoped receiver and the gateway.

A `secret` run on a controller without mediation is held with the reason
`ScopedDispatchDisabled`; it is never sent to a fleet parent.

## What the one switch wires

`celln.mediation.enabled=true` (requires `celln.fleet.enabled`) configures three
things against the same issuer, keyset, CA and tokens, or refuses to render:

| Component | What changes |
|---|---|
| Controller | `CELLN_SCOPED_CONFIG` points at a chart-rendered `config.json` (paths and names only). The issuer signing key and both transport tokens come from `controllerSecret`. |
| Fleet dispatchers (`celln-node`) | `--scoped-operator-token-file`, `--scoped-jwks-file`, `--scoped-issuer`, `--scoped-gateway-origin`, `--scoped-gateway-ca`, and `--scoped-parent-request-file` when the node has one. A small unprivileged `scoped-receiver` sidecar terminates TLS in front of the dispatcher. |
| Model gateway | Deployed from Secret/ConfigMap volumes; no configuration PVC. |

Why a TLS sidecar: the dispatcher speaks plaintext HTTP, and the controller only
talks to an HTTPS receiver with an explicit CA. The sidecar (`/celln-parent-proxy
--scoped-receiver`, shipped in the controller image) forwards nothing but
`POST /v1/scoped/{prepare,start,read,cleanup}` to the dispatcher on loopback.
The operator bearer and the signed permits remain the authority.

## 1. PostgreSQL (not bundled)

The gateway keeps budgets and registrations in PostgreSQL and refuses to start
without it. Apply `migrations/002_celln_model_budget.sql` then
`migrations/003_celln_model_gateway.sql`, and publish the connection URL:

```bash
kubectl -n sympozium-system create secret generic model-gateway-database \
  --from-literal=database-url='postgres://gateway:...@postgres.databases.svc:5432/gateway?sslmode=require'
```

A throwaway single-pod PostgreSQL is fine for evaluation. It holds accounting,
so do not use one for anything you need to keep.

## 2. Bootstrap the trust

Install the fleet first (`sympozium install --celln-fleet`, see
[Celln Fleet Installation](celln-fleet-installation.md)); then:

```bash
sympozium celln-mediation bootstrap --cluster-id my-cluster --values-out mediation-values.yaml
```

It mints, once, an Ed25519 issuer key and its public JWKS, two independent
bearer tokens, and a private CA with one server certificate each for the
gateway and the receiver (the CA key is discarded). It publishes:

| Object | Namespace | Keys | Read by |
|---|---|---|---|
| Secret `celln-mediation-controller` | control plane | `issuer.key` (PKCS#8 Ed25519 PEM), `receiver-token`, `gateway-token` | controller |
| Secret `celln-mediation-gateway` | control plane | `tls.crt`, `tls.key`, `registration-token` (= `gateway-token`) | gateway |
| Secret `celln-mediation-node` | `celln-system` | `operator-token` (= `receiver-token`), `tls.crt`, `tls.key` | dispatcher, receiver sidecar |
| ConfigMap `celln-mediation-trust` | both | `jwks.json`, `ca.crt` | all three |

The JWKS and CA certificate are public, so they live in a ConfigMap: they can be
inspected and rotated without Secret access, and the dispatcher and gateway
re-read the keyset (the gateway on `SIGHUP`). Nothing private is ever rendered
from Helm values, and the chart generates no keys.

A rerun verifies the five objects still belong together and changes nothing. It
never repairs or rotates by replacement: to rotate, delete all five
deliberately, bootstrap again and restart the three components. Certificates
last `--validity` (default one year). If your release is not named `sympozium`,
pass `--release-fullname`; if you set `celln.mediation.receiver.url`, pass its
host with `--receiver-host`.

You can also create these objects yourself; the table is the whole contract.
The JWKS is `{"keys":[{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"...","x":"..."}]}`.

## 3. Enable it

`mediation-values.yaml` from the bootstrap carries `clusterId`, `issuer.keyId`
and the object names. Add the gateway's operator inputs
(`charts/testdata/celln-mediation-values.yaml` is a complete sample):

```yaml
modelGateway:
  image: ghcr.io/sympozium-ai/sympozium/model-gateway@sha256:<digest>   # digest-pinned
  database:
    secretName: model-gateway-database
  namespaces: [team-a]        # optional, see "Gateway RBAC"
  egress: [...]               # reviewed: DNS, Kubernetes API, PostgreSQL, providers
```

```bash
helm upgrade sympozium charts/sympozium -n sympozium-system --reuse-values \
  -f mediation-values.yaml -f gateway-values.yaml
```

Rendering fails, naming the value, when anything is missing or contradictory:
no fleet, no `clusterId`/`issuer.keyId`, another issuer name, an empty object
name, no database Secret, an unpinned image, no egress list, a configuration
claim as well, or a receiver URL that is not an HTTPS origin.

Enabling (or disabling) mediation changes the `celln-node` pod template, so the
owners roll one node at a time and live native parents on each node end.

## 4. Verify

```bash
kubectl -n sympozium-system rollout status deploy/sympozium-model-gateway      # ready = PostgreSQL, RBAC and keys accepted
kubectl -n sympozium-system logs deploy/sympozium-controller-manager | grep -i "Scoped Celln"
kubectl -n celln-system logs ds/celln-node -c dispatcher | grep -i scoped
```

`/v1/scoped/*` no longer answers `404 scoped receiver disabled`. From the
controller's network identity, an unauthenticated request to the receiver now
gets `401` (run it from a debug pod labelled like the controller, or temporarily
admit your pod in the `celln-node-ingress` NetworkPolicy):

```bash
curl --cacert ca.crt -X POST https://celln-scoped-receiver.celln-system.svc:9443/v1/scoped/read -d '{}'
```

## The enduring parent request

Enduring scoped runs need `--scoped-parent-request-file`. Celln's
`starter-configure` writes `scoped-parent-request.json` beside
`native-template.json`, and the chart defaults `celln.mediation.parentRequestFile`
to that file in the first backend's configuration on the node
(`/var/lib/sympozium-celln/<scope>/configuration-<package>-<backend>/`).

Celln refuses to start when the flag names a missing file, so the owner pod
waits for that configuration to exist and passes the flag **only if the file is
there**. Without it the dispatcher logs that enduring scoped runs are disabled
and serves one-shot scoped dispatch. The file is read once at start: after it
appears (a newer starter package, or a reconfigured backend), restart the
`celln-node` pod on that node.

## Gateway RBAC

By default the gateway keeps its existing cluster-wide `get` on Secrets,
ModelConnections and Namespaces. Set `modelGateway.namespaces` to replace that
with one Role/RoleBinding per listed namespace plus a ClusterRole limited to
reading those Namespace objects; the chart then also sets the gateway's
`readinessNamespaces`, so readiness probes exactly that access. Add a namespace
to the list before its Agents use mediation.

## Current limits

- **One receiver node.** The router does not forward `/v1/scoped/*`, and a
  prepared operation lives on the node that prepared it. The default receiver is
  the `celln-scoped-receiver` Service, correct only while one `celln-node` pod
  exists. With several KVM nodes set `celln.mediation.receiver.url` to an HTTPS
  origin that reaches exactly one of them and bootstrap with its
  `--receiver-host`. No HA.
- **Chat only.** Borrowed workspace and HTTPS tools are refused on the mediated
  path.
- **A dispatcher restart loses live scoped parents**, including the roll caused
  by toggling mediation or upgrading the fleet package.
- **Rotation is manual** (delete, bootstrap, restart); there is no overlap
  window tooling yet, although the verifiers accept a multi-key JWKS.
- Secret volumes cannot be owned by a non-root user, and the controller and
  gateway refuse key or token files that are not owner-only. A non-root init
  step in each pod therefore copies the operator's files into an in-memory
  volume with mode `0400`; a changed Secret takes effect on the next pod restart.
- NetworkPolicies need an enforcing CNI. The gateway admits the controller and
  the `celln-node` pods on 8443; `celln-node` admits the controller on the
  receiver port only, and its plaintext 8787 stays router-only.
