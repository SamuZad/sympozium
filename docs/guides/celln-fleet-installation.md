# Celln fleet installation — enduring parents on every KVM node

The fleet is the multi-node form of the native Celln plane. One reviewed
starter package and one scope are installed once; after that, joining a node
is a single label and no namespace needs its own installation. It replaces
the single pinned dispatcher of the
[native installation](celln-native-installation.md), which remains the
small-cluster path.

What runs where:

| Component | Placement | Role |
| --- | --- | --- |
| `celln-node` DaemonSet | every node labeled `celln.dev/kvm=true` | init step prepares `/var/lib/sympozium-celln/<scope>` from the package; main container is the dispatcher serving `/v1/executions` and `/v1/parents` |
| `celln-router` | any node | gateway; discovers owners through the headless `celln-node` Service and binds each parent to the owner that issued it |
| controller | any node | issues parents through the gateway (`POST /v1/parents/provision`); keeps only its own journal/approvals on a `ReadWriteOnce` claim |

Nothing mounts a host path outside `/var/lib/sympozium-celln/<scope>`, the
controller mounts no host state at all, and no node is ever named in values.

## Trust and credentials

- **Publisher.** The package is signed with an operator seed. Its publisher key
  (from `celln starter-inspect`) is approved once in values; every node writes
  it into `trusted-closures.json` before admission. The package cannot install
  its own trust.
- **Parent principal.** `sympozium install --celln-fleet` generates one bearer
  credential for the gateway (`celln-router-parent`) and publishes only its
  BLAKE3 hash as the client policy every node installs. Owners never see the
  token; the controller never sees the policy. Rerunning the installer verifies
  the pair and refuses to rotate it by replacement.
- **Model credential.** The model profile references an absolute file path.
  In fleet mode that file is a Secret (`celln-fleet-model-credential`) mounted
  read-only into each dispatcher; guests and the controller cannot read it.
  This keeps the key inside the cluster trust boundary until the dedicated
  model gateway (#502) is attached to native parents; treat it as interim.
- **Configuration publication.** Only the DaemonSet's init step holds a
  projected ServiceAccount token, scoped to creating and reading one ConfigMap
  in `celln-system`. The dispatcher container has no API credential.

## Prerequisites

1. Nodes with `/dev/kvm`, a readable kernel under `/boot` with its
   `/lib/modules` directory (the dispatcher's readiness gate checks for one;
   Kind nodes ship without a kernel, so copy the host's in for development),
   and an enforcing CNI if you rely on the rendered NetworkPolicies.
2. A reviewed starter package, built **once** on any Linux host with the
   pinned Celln release (`config/celln/release.json`):

   ```sh
   celln starter-package --runtime-dir /opt/celln/runtime \
     --guest-dir /opt/celln/runtime/pilot --kernel /boot/REVIEWED_KERNEL \
     --signing-key /etc/celln-publisher/private.seed \
     --output /var/lib/celln-packages/starter-2026-09
   celln starter-inspect /var/lib/celln-packages/starter-2026-09   # packageHash, publisher
   ```

   Record `packageHash` and the bundle `publisher`. Guard the seed; it is
   never copied into the package or the cluster.
3. The package published as a digest-pinned OCI image whose only content is
   the package directory at `/package`:

   ```sh
   printf 'FROM scratch\nCOPY package /package\n' > Dockerfile
   cp -r /var/lib/celln-packages/starter-2026-09 package
   docker build -t registry.example/celln/starter:2026-09 . && docker push registry.example/celln/starter:2026-09
   docker inspect --format '{{index .RepoDigests 0}}' registry.example/celln/starter:2026-09
   ```

   Use the `repository@sha256:...` form; tags are refused. A private registry
   needs a `.dockerconfigjson` Secret in `celln-system` referenced by
   `celln.fleet.package.pullSecret`.
4. The model provider credential in a local file (one line, at least 24
   characters), or an existing `celln-fleet-model-credential` Secret in
   `celln-system`. Keyless backends such as llama-server need neither; see
   [Choosing the model backend](#choosing-the-model-backend).

## Install

```sh
sympozium install -n celln-agents --celln-fleet \
  --celln-fleet-scope starter \
  --celln-fleet-package-image registry.example/celln/starter@sha256:REVIEWED_DIGEST \
  --celln-fleet-package-hash blake3:REVIEWED_PACKAGE_JSON_HASH \
  --celln-fleet-publisher REVIEWED_PUBLISHER_KEY \
  --celln-fleet-model-credential-file /path/to/model-token \
  --celln-fleet-output-dir /ABS/PRIVATE/fleet-starter \
  --celln-native-approve-starter-tools
kubectl label node kvm-a kvm-b celln.dev/kvm=true
```

The command runs two phases and is safe to rerun:

1. Publishes the parent principal and model credential, then installs the
   chart with `celln.fleet.*` set. Labeled nodes pull the package by digest,
   verify `package.json` against the hash, run `starter-admit` (real guest
   member checks on KVM) and `starter-configure`, and the first node publishes
   `catalogue.json`, `configured.json` and `native-template.json` as the
   `celln-fleet-configuration` ConfigMap. Later nodes verify they derived
   identical files; a different package under the same scope is refused.
2. Waits (default 15 minutes, `--celln-fleet-wait`) for that ConfigMap,
   materializes it under the output directory, installs the catalogue and
   grant layers in the target namespace exactly as `celln-tool install-native`
   does, and publishes `celln-parent-config` with a `remoteProvisioner`. The
   final upgrade wires the controller and creates the journal claim.

If the wait expires, label a node, inspect the `prepare` init container's log
in `celln-system`, and rerun the same command: existing trust, credential,
configuration and installation records are verified, never replaced.

## Choosing the model backend

The scope has one model backend, set at install time and configured on every
node. DeepSeek is the default; pass `--celln-fleet-model-provider` for another:

| Backend | Flags | Credential |
| --- | --- | --- |
| DeepSeek | none (model `deepseek-chat`) | `--celln-fleet-model-credential-file` with the API key |
| OpenAI | `--celln-fleet-model-provider openai --celln-fleet-model MODEL` | the OpenAI API key |
| Anthropic | `--celln-fleet-model-provider anthropic --celln-fleet-model MODEL` | the Anthropic API key |
| llama-server | `--celln-fleet-model-provider llama-server --celln-fleet-model MODEL.gguf --celln-fleet-model-endpoint http://HOST:8080/v1/chat/completions --celln-fleet-model-allow-insecure` | none |

Any other OpenAI- or Anthropic-compatible service works with a custom
provider name plus `--celln-fleet-model-endpoint` and
`--celln-fleet-model-protocol openai-chat|anthropic-messages`.

- The endpoint must be reachable from the KVM nodes. Use an IP address for a
  host on a VPN or LAN if the cluster cannot resolve its name.
- Plain HTTP or a private address needs `--celln-fleet-model-allow-insecure`.
  The approval is recorded on the node's model profile and on each tenant's
  connection; a cluster Secret is never sent over plain HTTP.
- A model request may run for the whole turn, so slow local models are fine
  within the turn deadline.
- The backend is fixed for a scope. To change it, install a new scope.

## Leases and budgets

A parent lives for its run's `leaseSeconds` and may spend up to its run's
turn, model-request and output-token budget. The scope's ceilings bound
every run and are configured on every node at install time:

| Flag | Default | Range |
| --- | --- | --- |
| `--celln-fleet-max-lease-seconds` | 86400 (24 h) | 60–86400 |
| `--celln-fleet-max-turns` | 256 | 1–1024 |
| `--celln-fleet-max-model-requests` | 768 | 3–6144 |
| `--celln-fleet-max-output-tokens` | 393216 | 1536–3145728 |

A new conversation asks for a working session inside those ceilings by
default (four hours, 64 turns, 192 requests, 98304 tokens; the API reports
them per profile as `sessionDefaults`). A run asking for more than a ceiling
is refused with `AUTH_LIMIT_RANGE`. When a lease ends no new turn is admitted
and the parent stops; the conversation view shows the deadline and asks for a
new conversation. Leases are not extended in place. Every live parent holds
two cells and its declared memory for its whole lease, so long defaults cost
node capacity while conversations sit idle.

## Authorising namespaces

The installer publishes the reviewed starter configuration **once per scope**
as cluster-scoped objects — `CellnRuntimeProfile` `celln-native-<scope>`
(carrying the native parent/worker material), three `ClusterCellnTool`s
`celln-<scope>-<tool>`, and a `CellnExecutionPolicy` `celln-fleet-<scope>`
with a `host-profile` model route and the reviewed ceilings.

**By default every namespace is authorised** except the system exclusions
(`kube-system`, `kube-public`, `kube-node-lease`, `cert-manager`, the chart's
namespace and `celln-system`) and any namespace labeled
`celln.sympozium.ai/excluded=true`. Fence a namespace off with that label;
nothing else is needed to admit one. Pass `--celln-fleet-authorise=labeled`
to invert this for regulated clusters: then only namespaces labeled
`celln.sympozium.ai/scope=<scope>` are admitted and the installer labels the
`-n` namespace for you.

A namespace's runs select three ordinary workload objects — an `AgentRuntime`
wrapper `celln-native` referencing the profile, an `Agent` `celln-agent` and
a host-profile `ModelConnection` `celln-native`. **They are created on first
use**: the Agent wizard offers the platform profile on the Celln plane in any
authorised namespace and creates the wrappers when you finish, and the API
exposes the same step for automation:

```sh
curl -H "Authorization: Bearer $TOKEN" "$API/api/v1/celln-platform/profiles?namespace=team-b"
curl -H "Authorization: Bearer $TOKEN" -X POST -H 'Content-Type: application/json' \
  -d '{"profile":"celln-native-<scope>"}' "$API/api/v1/celln-platform/wrappers?namespace=team-b"
```

Existing objects are never modified, so a namespace that prefers to manage
its own wrappers in Git can apply them instead (copy them from the install
namespace with `kubectl -n <install-ns> get modelconnection,agentruntime,agent -o yaml`).
No per-namespace install, grant ConfigMaps or copied tools. Enduring runs in
that namespace select `runtimeRef: celln-native`, `clusterToolRefs` from the
shared catalogue, `model.connectionRef: celln-native` and the profile's
persona; the controller resolves policy, profile, tools and route into one
immutable decision, issues the parent through the gateway, and re-checks that
authority before every later turn. A run in an excluded namespace is held
with `CellnParentReady=AdmissionPending (AUTH_POLICY_WITHDRAWN)`; nothing is
issued. The UI wizard offers wrapper runtimes on the Celln plane, lists the
shared catalogue for them and uses the namespace's host-profile
`ModelConnection` instead of asking for a key.

The `ModelConnection`'s `credentialProfile` names the owner-installed model
credential (the scope); `CellnExecutionPolicy` routes with `auth: host-profile`
are the interim boundary until the model gateway (P1) attaches to native
parents and routes switch to `auth: secret`.

## Verify

```sh
kubectl -n celln-system get daemonset celln-node            # DESIRED = labeled nodes
kubectl -n celln-system get pods -l app.kubernetes.io/name=celln-node -o wide
kubectl -n celln-system logs -l app.kubernetes.io/name=celln-router | grep backends
```

Then create an enduring run in an authorised namespace with the shared starter
tools; the installer leaves a ready-made `run.json` under the output directory
(change its namespace for another authorised namespace).
The run's `status.cellnParent.binding.target` is the gateway; the gateway's
`provisions` and `parents` ledgers on the ownership claim record which owner
holds it, and that owner's `authority/parent-journal` carries the incarnation.
Follow-up turns created by hand must carry the run's controller
`ownerReference` (the API server adds it for you); a turn without one is
refused as unbound. `test/integration/test-celln-fleet.sh` runs the whole
journey on a three-node Kind cluster, including a node-leave drain.

## Operations

- **Join a node:** label it. The package is admitted on that node only.
- **Leave a node:** remove the label or drain it. The DaemonSet pod's preStop
  calls `/v1/drain`, which closes admission, stops every parent tree on that
  owner and confirms teardown; those runs report `ContextLost`. Once the
  address has left the fleet the gateway answers `original parent backend
  removed` for its identities, which the controller also treats as context
  loss (and as established teardown when the run is deleted); nothing is
  re-placed.
- **Rolling updates** replace one node's dispatcher at a time with the same
  drain semantics. A dispatcher restart loses live parents on that node.
- **New package:** use a new scope. One scope carries exactly one package
  hash; nodes refuse to publish a differing configuration.
- **Uninstall** retains `/var/lib/sympozium-celln/<scope>` on each node and the
  journal claim; remove them only after every run has been deleted and
  cleanup confirmed.

## Capacity

Each node sizes itself when its dispatcher starts (`celln.fleet.capacity:
auto`, the default): it may reserve `memoryPercent` (75) of the node's memory
— the container's cgroup limit when one is set — for guests, allows one cell
per `cellMemoryBytes` (640 MiB) up to `maxCellsCeiling`, and one broker slot
per cell. The dispatcher logs the result (`celln capacity: node=… cells=…`).
A native parent charges two cells, two broker slots and its declared memory
(1.25 GiB for the starter profile). For nominal node sizes (the kernel reports
slightly less, so real numbers come out a little lower):

| Node memory | Cells | Parents per node |
| --- | --- | --- |
| 16 GiB | 19 | 9 |
| 64 GiB | 76 | 38 |
| 256 GiB | 307 | 153 |

Set `capacity: fixed` with `maxCells`, `memoryBytes` and `egressSlots` to pin
exact numbers, or lower `memoryPercent` on nodes that run other workloads.

Per-parent broker charging arrived in Celln v0.5.12 (celln#112), which this
chart pins; releases before it hold one parent per node whatever the budget says.

The gateway provisions each new parent on the healthy owner with the most
spare cells (then memory), as the owners advertise on `/v1/health`; equally
free owners are chosen in hash order, so placement is deterministic. Once
provisioned, a parent is bound to its owner. An owner that still refuses a
create ends that run with `CellnParentReady` reason `CreateRefused` ("create a
new run"); the incarnation is never retried and the run deletes cleanly.

## Limits

Single active turn per parent, no parent migration or checkpoint recovery,
no live lease extension, and the model credential Secret mounted into every
dispatcher is an interim boundary until the model gateway attaches to native
parents (#464 "Path to production"). One model backend per scope; persona and
tool set are fixed by the starter package (#535).
